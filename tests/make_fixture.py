#!/usr/bin/env python3
"""make_fixture.py — synthetic Cvent fixture + mock upstream for test_server.sh.

One file, stdlib only. Two jobs:

1. Fixture generator: `python3 make_fixture.py --out DIR` writes the same
   dump layout the Go server's --dump mode produces, so the identical data
   works for the mock AND later for static mode:

     DIR/index.json                     [ {code, title, start, end, pulledAt} ]
     DIR/<code>/event.json              the event object
     DIR/<code>/meta.json               {code, pulledAt, counts}
     DIR/<code>/<13 resource files>     raw JSON arrays (literal null when
                                        empty — matches server/dump.go)

2. Mock Cvent API: `python3 make_fixture.py --out DIR --serve PORT` (blocks).
   The mock replays the fixture over the EXACT endpoint set server/event.go
   fans out to, with single-page pagination (paging.totalCount set, no
   paging.currentToken — the Go walkPages loop stops on that). Stateless:
   every request returns the same fixture data regardless of order. Each
   request path is logged to stderr (test_server.sh greps it to prove no
   request ever reaches the real api-platform.cvent.com host).

The fixture holds TWO events so the multi-event API (event-scoped routes +
the /api/cvent/events catalog) is exercised end to end:
  syn-event     the original synthetic event (ground truth below)
  syn-event-2   a second, smaller event (1 attendee, 1 paid order) — the
                mock resolves it by code and serves its own resources

Fixture ground truth (the payments test hardcodes these — keep in sync):
  orders:      o1 paid    100.00/100.00/due 0.00   (att-001, via order.attendee.id)
               o2 partial 200.00/ 50.00/due 150.00 (att-003, via order.attendeeId)
               o3 unpaid   300.00/  0.00/due 300.00 (att-004, via order.contact.id)
  transactions: 100.00 success, 50.00 success, 25.00 success Refund
  expected totals: ordered 600.00, paid 150.00, due 450.00, refunded 25.00
  q=zeta matches exactly 2 attendees: att-001 (lastName "Zetar") and
  att-002 (contact.email "zeta@example.org")
"""

import argparse
import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PULLED_AT = "2026-09-22T00:00:00Z"

# Resource names exactly as server/event.go names them (bundle key == counts
# key == dump file key in server/dump.go's dumpResources).
RESOURCE_KEYS = [
    "attendees", "activities", "orders", "orderItems", "transactions",
    "transactionItems", "feeItems", "admissionItems", "registrationTypes",
    "discounts", "sessions", "speakers", "eventQuestions",
]

# dump file name per resource key (server/dump.go layout; same names
# tests/pull_event.sh writes).
RESOURCE_FILES = {
    "attendees": "attendees.json",
    "activities": "activities.json",
    "orders": "orders.json",
    "orderItems": "order-items.json",
    "transactions": "transactions.json",
    "transactionItems": "transaction-items.json",
    "feeItems": "fee-items.json",
    "admissionItems": "admission-items.json",
    "registrationTypes": "registration-types.json",
    "discounts": "discounts.json",
    "sessions": "sessions.json",
    "speakers": "speakers.json",
    "eventQuestions": "event-questions.json",
}


def _event(code, uuid, title):
    return {
        "id": uuid,
        "title": title,
        "code": code,
        "virtual": False,
        "format": "In-person",
        "start": "2027-05-23T12:00:00.000Z",
        "end": "2027-05-28T21:00:00.000Z",
        "timezone": "America/New_York",
        "currency": "USD",
        "status": "Active",
        "eventStatus": "Upcoming",
        "planningStatus": "Active",
        "testMode": True,
        "created": "2026-09-01T00:00:00Z",
        "lastModified": "2026-09-20T00:00:00Z",
        "_links": {},
    }


def _attendee(uuid, title, aid, first, last, conf, email=None, placement="name"):
    """One attendee row. `placement` varies WHERE the name parts live, to
    exercise server/handlers.go's lenient name resolution (name object ->
    top-level -> contact). contact.email is present for 2 attendees."""
    a = {
        "id": aid,
        "confirmationNumber": conf,
        "checkedIn": False,
        "event": {"id": uuid, "name": title},
        "registrationPath": {"id": "rt-1", "name": "General Admission"},
    }
    parts = {"firstName": first, "lastName": last}
    if placement == "name":
        a["name"] = parts
    elif placement == "top":
        a.update(parts)
    elif placement == "contact":
        a["contact"] = dict(parts)
    if email is not None:
        a.setdefault("contact", {})["email"] = email
    return a


def _build_syn_event():
    """The original synthetic event's 13 resources (ground truth in the
    module docstring)."""
    uuid = "00000000-1111-4222-8333-000000000001"
    title = "Synthetic Test Event"
    attendees = [
        _attendee(uuid, title, "att-001", "Alice", "Zetar", "CONF-0001"),
        _attendee(uuid, title, "att-002", "Bob", "Smith", "CONF-0002",
                  email="zeta@example.org", placement="top"),
        _attendee(uuid, title, "att-003", "Carol", "Jones", "CONF-0003",
                  email="carol@example.org", placement="contact"),
        _attendee(uuid, title, "att-004", "Dan", "Okafor", "CONF-0004"),
        _attendee(uuid, title, "att-005", "Erin", "Kowalski", "CONF-0005"),
    ]
    # Registration answers reference questions by id only; eventQuestions
    # resolves them (q-sys is an SL_* system field nobody answers).
    attendees[0]["answers"] = [
        {"question": {"id": "q-terms"}, "value": ["I accept the terms."]},
        {"question": {"id": "q-first"}, "value": ["Yes"]},
    ]
    event_questions = [
        {"id": "q-terms", "text": "Terms and Conditions", "type": "MultiChoice", "event": {"id": uuid}},
        {"id": "q-first", "text": "First time attending", "type": "SingleChoice", "event": {"id": uuid}},
        {"id": "q-sys", "text": "SL_DURATION11", "type": "OpenEndedTextOneLine", "event": {"id": uuid}},
    ]

    ev = {"id": uuid}
    orders = [
        # paid: due 0 -> status "paid"; id via order.attendee.id
        {
            "id": "ord-1", "invoiceNumber": "INV-1001", "type": "Registration",
            "attendee": {"id": "att-001"},
            "amountOrdered": 100.0, "amountPaid": 100.0, "amountDue": 0.0,
            "paymentMethod": "CREDIT_CARD", "cancelled": False, "event": ev,
        },
        # partial: 0 < due < ordered -> "partial"; id via top-level attendeeId
        {
            "id": "ord-2", "invoiceNumber": "INV-1002", "type": "Registration",
            "attendeeId": "att-003",
            "amountOrdered": 200.0, "amountPaid": 50.0, "amountDue": 150.0,
            "paymentMethod": "CREDIT_CARD", "cancelled": False, "event": ev,
        },
        # unpaid: due == ordered -> "unpaid"; id via order.contact.id
        {
            "id": "ord-3", "invoiceNumber": "INV-1003", "type": "Registration",
            "contact": {"id": "att-004"},
            "amountOrdered": 300.0, "amountPaid": 0.0, "amountDue": 300.0,
            "paymentMethod": "CREDIT_CARD", "cancelled": False, "event": ev,
        },
    ]
    order_items = [
        {
            "id": "oi-1", "order": {"id": "ord-1"}, "name": "General Admission",
            "amount": 100.0, "currency": "USD",
            "admissionItem": {"id": "adm-1", "name": "Standard Badge"},
            "event": ev,
        },
        {
            "id": "oi-2", "order": {"id": "ord-2"}, "name": "General Admission",
            "amount": 200.0, "currency": "USD",
            "admissionItem": {"id": "adm-1", "name": "Standard Badge"},
            "event": ev,
        },
    ]
    transactions = [
        # success purchase matching ord-1
        {"id": "txn-1", "success": True, "paymentType": "CreditCard",
         "type": "PURCHASE", "amount": 100.0,
         "order": {"id": "ord-1"}, "event": ev},
        # success purchase matching ord-2 (partial)
        {"id": "txn-2", "success": True, "paymentType": "CreditCard",
         "type": "PURCHASE", "amount": 50.0,
         "order": {"id": "ord-2"}, "event": ev},
        # success refund -> counted in totals.refunded ONLY (never in
        # ordered/paid/due, which payments.go sums from orders alone)
        {"id": "txn-3", "success": True, "paymentType": "Refund",
         "type": "REFUND", "amount": 25.0,
         "order": {"id": "ord-1"}, "event": ev},
    ]
    transaction_items = [
        {"id": "ti-1", "transaction": {"id": "txn-3"}, "name": "General Admission",
         "amount": -25.0, "currency": "USD", "event": ev},
    ]

    def rt(rid, name, code):
        return {
            "id": rid, "name": name, "code": code, "description": name,
            "virtual": False, "openForRegistration": True,
            "event": {"id": uuid},
            "capacity": {"remaining": -1, "consumed": 0, "total": -1},
        }

    registration_types = [rt("rt-1", "General Admission", "GA"),
                          rt("rt-2", "Student", "STU")]

    def adm(aid, name):
        return {
            "id": aid, "name": name, "code": name, "description": name,
            "event": {"id": uuid},
        }

    admission_items = [adm("adm-1", "Standard Badge"), adm("adm-2", "VIP Badge")]

    def fee(fid, name, amount, product):
        return {
            "id": fid, "name": name, "amount": amount, "currency": "USD",
            "product": product, "active": True, "default": False,
            "created": "2026-09-01T00:00:00Z",
        }

    fee_items = [
        fee("fee-1", "Standard Rate", 300.0,
            {"id": "adm-1", "type": "AdmissionItem", "name": "Standard Badge"}),
        fee("fee-2", "VIP Rate", 500.0,
            {"id": "adm-2", "type": "AdmissionItem", "name": "VIP Badge"}),
    ]

    discounts = [{
        "id": "disc-1", "name": "Early Bird", "code": "EARLY",
        "type": "DISCOUNT_CODE", "active": True, "stackable": False,
        "method": {"type": "BY_PERCENTAGE", "value": 10.0},
        "audienceType": "ALL", "level": "ADMISSION", "event": ev,
    }]

    sessions = [{
        "id": "ses-1", "name": "Opening Session", "type": "Regular",
        "start": "2027-05-23T13:00:00.000Z", "end": "2027-05-23T14:00:00.000Z",
        "event": ev,
    }]
    speakers = [{
        "id": "spk-1", "name": "Dr. Test Speaker", "affiliation": "Example U",
        "event": ev,
    }]

    # activities: intentionally empty (count key still present, value 0)
    return {
        "attendees": attendees,
        "activities": [],
        "orders": orders,
        "orderItems": order_items,
        "transactions": transactions,
        "transactionItems": transaction_items,
        "feeItems": fee_items,
        "admissionItems": admission_items,
        "registrationTypes": registration_types,
        "discounts": discounts,
        "sessions": sessions,
        "speakers": speakers,
        "eventQuestions": event_questions,
    }


def _build_syn_event_2():
    """A second, smaller event: 1 attendee, 1 fully-paid order, 1 session,
    1 speaker. Exercises the event-scoped routes with DIFFERENT data so a
    test that accidentally reads the wrong event's bundle fails loudly."""
    uuid = "00000000-2222-4333-8444-000000000002"
    title = "Synthetic Test Event 2"
    ev = {"id": uuid}
    attendees = [
        _attendee(uuid, title, "att-201", "Frank", "Grant", "CONF-2001"),
    ]
    orders = [
        {
            "id": "ord-21", "invoiceNumber": "INV-2001", "type": "Registration",
            "attendee": {"id": "att-201"},
            "amountOrdered": 400.0, "amountPaid": 400.0, "amountDue": 0.0,
            "paymentMethod": "CREDIT_CARD", "cancelled": False, "event": ev,
        },
    ]
    order_items = [
        {
            "id": "oi-21", "order": {"id": "ord-21"}, "name": "VIP Pass",
            "amount": 400.0, "currency": "USD",
            "admissionItem": {"id": "adm-21", "name": "VIP Pass"},
            "event": ev,
        },
    ]
    transactions = [
        {"id": "txn-21", "success": True, "paymentType": "CreditCard",
         "type": "PURCHASE", "amount": 400.0,
         "order": {"id": "ord-21"}, "event": ev},
    ]
    transaction_items = [
        {"id": "ti-21", "transaction": {"id": "txn-21"}, "name": "VIP Pass",
         "amount": 400.0, "currency": "USD", "event": ev},
    ]
    registration_types = [{
        "id": "rt-21", "name": "VIP", "code": "VIP", "description": "VIP",
        "virtual": False, "openForRegistration": True,
        "event": {"id": uuid},
        "capacity": {"remaining": -1, "consumed": 0, "total": -1},
    }]
    admission_items = [{
        "id": "adm-21", "name": "VIP Pass", "code": "VIP Pass",
        "description": "VIP Pass", "event": {"id": uuid},
    }]
    fee_items = [{
        "id": "fee-21", "name": "VIP Rate", "amount": 400.0, "currency": "USD",
        "product": {"id": "adm-21", "type": "AdmissionItem", "name": "VIP Pass"},
        "active": True, "default": False,
        "created": "2026-09-01T00:00:00Z",
    }]
    discounts = []
    sessions = [{
        "id": "ses-21", "name": "Sponsor Mixer", "type": "Regular",
        "start": "2027-05-24T13:00:00.000Z", "end": "2027-05-24T15:00:00.000Z",
        "event": ev,
    }]
    speakers = [{
        "id": "spk-21", "name": "Dr. Second Speaker", "affiliation": "Other U",
        "event": ev,
    }]
    return {
        "attendees": attendees,
        "activities": [],
        "orders": orders,
        "orderItems": order_items,
        "transactions": transactions,
        "transactionItems": transaction_items,
        "feeItems": fee_items,
        "admissionItems": admission_items,
        "registrationTypes": registration_types,
        "discounts": discounts,
        "sessions": sessions,
        "speakers": speakers,
        "eventQuestions": [],
    }


# code -> (event object, resources dict). The mock and the dump both read
# from this single source of truth.
EVENTS = {
    "syn-event": (
        _event("syn-event", "00000000-1111-4222-8333-000000000001",
               "Synthetic Test Event"),
        _build_syn_event(),
    ),
    "syn-event-2": (
        _event("syn-event-2", "00000000-2222-4333-8444-000000000002",
               "Synthetic Test Event 2"),
        _build_syn_event_2(),
    ),
}

# uuid -> code (inverse map for the mock's per-uuid resource routes).
UUID_TO_CODE = {ev["id"]: code for code, (ev, _res) in EVENTS.items()}


def _counts(resources):
    return {k: len(v) for k, v in resources.items()}


# --- dump layout (matches server/dump.go) -----------------------------------

def write_dump(out_dir):
    os.makedirs(out_dir, exist_ok=True)
    index = []
    for code, (event, resources) in EVENTS.items():
        counts = _counts(resources)
        code_dir = os.path.join(out_dir, code)
        os.makedirs(code_dir, exist_ok=True)

        def write(name, data, _code_dir=code_dir):
            # `data` is either a Python object (serialized) or a pre-built
            # string (e.g. the literal "null" for empty resources — Go parity).
            text = data if isinstance(data, str) else json.dumps(data, indent=1)
            if not text.endswith("\n"):
                text += "\n"
            with open(os.path.join(_code_dir, name), "w") as f:
                f.write(text)

        write("event.json", event)
        # one file per resource: raw array, or literal null when empty
        for key in RESOURCE_KEYS:
            rows = resources[key]
            write(RESOURCE_FILES[key], rows if rows else "null")
        write("meta.json", json.dumps(
            {"code": code, "pulledAt": PULLED_AT, "counts": counts}, indent=2))

        index.append({
            "code": code,
            "title": event["title"],
            "start": event["start"],
            "end": event["end"],
            "pulledAt": PULLED_AT,
        })

    # sort by start date, then title (matches server/dump.go's upsert order)
    index.sort(key=lambda e: (e["start"], e["title"]))
    with open(os.path.join(out_dir, "index.json"), "w") as f:
        json.dump(index, f, indent=2)
        f.write("\n")


# --- mock Cvent API ----------------------------------------------------------

def _page(items):
    """Single-page Cvent list response: items + paging with totalCount and NO
    currentToken (the Go walkPages loop stops on the missing token)."""
    return {
        "data": items,
        "paging": {"limit": 200, "totalCount": len(items)},
    }


_UUID_RE = re.compile(r"event\.id eq '([0-9a-fA-F-]+)'")


def _resources_for_uuid(uuid):
    code = UUID_TO_CODE.get(uuid)
    return EVENTS[code][1] if code is not None else None


def make_handler():
    class MockCvent(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, format, *args):  # noqa: A002 - stdlib signature
            # every request path -> stderr (test_server.sh greps this log)
            sys.stderr.write("%s %s\n" % (self.command, self.path))
            sys.stderr.flush()

        def _send(self, obj, status=200):
            body = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _not_found(self):
            self._send({"error": "mock: no such endpoint: %s" % self.path}, 404)

        def _drain_body(self):
            try:
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    return self.rfile.read(n).decode()
            except (ValueError, OSError):
                pass
            return ""

        def do_POST(self):
            path = urlparse(self.path).path
            body = self._drain_body()  # keep-alive framing stays clean
            if path == "/oauth2/token":
                # accept any Basic auth (the test uses test:test)
                self._send({"access_token": "mock-token", "token_type": "bearer",
                            "expires_in": 3600})
                return
            # The filter endpoints carry the event uuid in the JSON body
            # ("event.id eq '<uuid>'"); recover it to pick the right event.
            m = _UUID_RE.search(body)
            res = _resources_for_uuid(m.group(1)) if m else None
            table = {
                "/admission-items/filter": "admissionItems",
                "/sessions/filter": "sessions",
                "/speakers/filter": "speakers",
                "/attendees/filter": "attendees",
            }
            if path in table:
                self._send(_page(res[table[path]] if res else []))
                return
            self._not_found()

        def do_GET(self):
            parsed = urlparse(self.path)
            path, q = parsed.path, parse_qs(parsed.query)
            if path == "/events":
                # code -> uuid resolution: single-page list, first item wins.
                # The Go client sends filter=code eq '<code>'&limit=200.
                filt = (q.get("filter") or [""])[0]
                matched = [ev for code, (ev, _res) in EVENTS.items()
                           if ("code eq '%s'" % code) in filt]
                self._send(_page(matched))
                return
            # per-uuid resource routes: /events/<uuid>/<resource>
            m = re.match(r"^/events/([0-9a-fA-F-]+)/(.+)$", path)
            if m:
                res = _resources_for_uuid(m.group(1))
                if res is None:
                    self._not_found()
                    return
                table = {
                    "registration-types": "registrationTypes",
                    "fee-items": "feeItems",
                    "discounts": "discounts",
                    "orders": "orders",
                    "orders/items": "orderItems",
                    "transactions": "transactions",
                    "transactions/items": "transactionItems",
                }
                if m.group(2) in table:
                    self._send(_page(res[table[m.group(2)]]))
                    return
            if path == "/event-questions":
                filt = (q.get("filter") or [""])[0]
                mm = _UUID_RE.search(filt)
                res = _resources_for_uuid(mm.group(1)) if mm else None
                self._send(_page(res["eventQuestions"] if res else []))
                return
            if path == "/attendees/activities":
                # filter arrives as a QUERY PARAM here (not a POST body)
                filt = (q.get("filter") or [""])[0]
                mm = _UUID_RE.search(filt)
                res = _resources_for_uuid(mm.group(1)) if mm else None
                self._send(_page(res["activities"] if res else []))
                return
            self._not_found()

    return MockCvent


def serve(port):
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler())
    sys.stderr.write("mock cvent upstream listening on 127.0.0.1:%d (events %s)\n"
                     % (port, ", ".join(EVENTS)))
    sys.stderr.flush()
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", help="write the dump-layout fixture into DIR")
    ap.add_argument("--serve", type=int, help="run the mock Cvent API on PORT (blocks)")
    args = ap.parse_args()

    if args.out:
        write_dump(args.out)
        sys.stderr.write("fixture written to %s/ (%s events +index.json)\n"
                         % (args.out, ", ".join(EVENTS)))
    if args.serve:
        serve(args.serve)
    if not args.out and not args.serve:
        ap.print_usage()
        sys.exit(2)


if __name__ == "__main__":
    main()
