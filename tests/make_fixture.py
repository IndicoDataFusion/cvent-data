#!/usr/bin/env python3
"""make_fixture.py — synthetic Cvent fixture + mock upstream for test_server.sh.

One file, stdlib only. Two jobs:

1. Fixture generator: `python3 make_fixture.py --out DIR` writes the same
   dump layout the Go server's --dump mode produces, so the identical data
   works for the mock AND later for static mode:

     DIR/index.json                     [ {code, title, start, end, pulledAt} ]
     DIR/syn-event/event.json           the event object
     DIR/syn-event/meta.json            {code, pulledAt, counts}
     DIR/syn-event/<12 resource files>  raw JSON arrays (literal null when
                                        empty — matches server/dump.go)

2. Mock Cvent API: `python3 make_fixture.py --out DIR --serve PORT` (blocks).
   The mock replays the fixture over the EXACT endpoint set server/event.go
   fans out to, with single-page pagination (paging.totalCount set, no
   paging.currentToken — the Go walkPages loop stops on that). Stateless:
   every request returns the same fixture data regardless of order. Each
   request path is logged to stderr (test_server.sh greps it to prove no
   request ever reaches the real api-platform.cvent.com host).

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
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

EVENT_CODE = "syn-event"
EVENT_UUID = "00000000-1111-4222-8333-000000000001"
PULLED_AT = "2026-09-22T00:00:00Z"

# Resource names exactly as server/event.go names them (bundle key == counts
# key == dump file key in server/dump.go's dumpResources).
RESOURCE_KEYS = [
    "attendees", "activities", "orders", "orderItems", "transactions",
    "transactionItems", "feeItems", "admissionItems", "registrationTypes",
    "discounts", "sessions", "speakers",
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
}


def _event():
    return {
        "id": EVENT_UUID,
        "title": "Synthetic Test Event",
        "code": EVENT_CODE,
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


def _attendee(aid, first, last, conf, email=None, placement="name"):
    """One attendee row. `placement` varies WHERE the name parts live, to
    exercise server/handlers.go's lenient name resolution (name object ->
    top-level -> contact). contact.email is present for 2 attendees."""
    a = {
        "id": aid,
        "confirmationNumber": conf,
        "checkedIn": False,
        "event": {"id": EVENT_UUID, "name": "Synthetic Test Event"},
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


def _build_resources():
    """The 12 bundle resources as plain Python lists (the mock's replay data
    and the dump's on-disk arrays)."""
    attendees = [
        _attendee("att-001", "Alice", "Zetar", "CONF-0001"),
        _attendee("att-002", "Bob", "Smith", "CONF-0002",
                  email="zeta@example.org", placement="top"),
        _attendee("att-003", "Carol", "Jones", "CONF-0003",
                  email="carol@example.org", placement="contact"),
        _attendee("att-004", "Dan", "Okafor", "CONF-0004"),
        _attendee("att-005", "Erin", "Kowalski", "CONF-0005"),
    ]

    ev = {"id": EVENT_UUID}
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
            "event": {"id": EVENT_UUID},
            "capacity": {"remaining": -1, "consumed": 0, "total": -1},
        }

    registration_types = [rt("rt-1", "General Admission", "GA"),
                          rt("rt-2", "Student", "STU")]

    def adm(aid, name):
        return {
            "id": aid, "name": name, "code": name, "description": name,
            "event": {"id": EVENT_UUID},
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
    }


def _counts(resources):
    return {k: len(v) for k, v in resources.items()}


# --- dump layout (matches server/dump.go) -----------------------------------

def write_dump(out_dir):
    event = _event()
    resources = _build_resources()
    counts = _counts(resources)
    code_dir = os.path.join(out_dir, EVENT_CODE)
    os.makedirs(code_dir, exist_ok=True)

    def write(name, data):
        # `data` is either a Python object (serialized) or a pre-built string
        # (e.g. the literal "null" for empty resources — Go parity).
        text = data if isinstance(data, str) else json.dumps(data, indent=1)
        if not text.endswith("\n"):
            text += "\n"
        with open(os.path.join(code_dir, name), "w") as f:
            f.write(text)

    write("event.json", event)
    # one file per resource: raw array, or literal null when empty (Go parity)
    for key in RESOURCE_KEYS:
        rows = resources[key]
        write(RESOURCE_FILES[key], rows if rows else "null")
    write("meta.json", json.dumps(
        {"code": EVENT_CODE, "pulledAt": PULLED_AT, "counts": counts}, indent=2))

    index = [{
        "code": EVENT_CODE,
        "title": event["title"],
        "start": event["start"],
        "end": event["end"],
        "pulledAt": PULLED_AT,
    }]
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


def make_handler(resources):
    event = _event()

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

        def do_POST(self):
            parsed = urlparse(self.path)
            path = parsed.path
            # drain the body so keep-alive framing stays clean
            try:
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    self.rfile.read(n)
            except (ValueError, OSError):
                pass
            if path == "/oauth2/token":
                # accept any Basic auth (the test uses test:***
                self._send({"access_token": "mock-token", "token_type": "bearer",
                            "expires_in": 3600})
                return
            if path == "/admission-items/filter":
                self._send(_page(resources["admissionItems"]))
                return
            if path == "/sessions/filter":
                self._send(_page(resources["sessions"]))
                return
            if path == "/speakers/filter":
                self._send(_page(resources["speakers"]))
                return
            if path == "/attendees/filter":
                self._send(_page(resources["attendees"]))
                return
            self._not_found()

        def do_GET(self):
            parsed = urlparse(self.path)
            path, q = parsed.path, parse_qs(parsed.query)
            if path == "/events":
                # code -> uuid resolution: single-page list, first item wins.
                # The Go client sends filter=code eq '<code>'&limit=200.
                filt = (q.get("filter") or [""])[0]
                if ("code eq '%s'" % EVENT_CODE) in filt:
                    self._send(_page([event]))
                else:
                    self._send(_page([]))
                return
            if path == "/events/%s/registration-types" % EVENT_UUID:
                self._send(_page(resources["registrationTypes"]))
                return
            if path == "/events/%s/fee-items" % EVENT_UUID:
                self._send(_page(resources["feeItems"]))
                return
            if path == "/events/%s/discounts" % EVENT_UUID:
                self._send(_page(resources["discounts"]))
                return
            if path == "/events/%s/orders" % EVENT_UUID:
                self._send(_page(resources["orders"]))
                return
            if path == "/events/%s/orders/items" % EVENT_UUID:
                self._send(_page(resources["orderItems"]))
                return
            if path == "/events/%s/transactions" % EVENT_UUID:
                self._send(_page(resources["transactions"]))
                return
            if path == "/events/%s/transactions/items" % EVENT_UUID:
                self._send(_page(resources["transactionItems"]))
                return
            if path == "/attendees/activities":
                # filter arrives as a QUERY PARAM here (not a POST body)
                self._send(_page(resources["activities"]))
                return
            self._not_found()

    return MockCvent


def serve(port):
    resources = _build_resources()
    httpd = ThreadingHTTPServer(("127.0.0.1", port), make_handler(resources))
    sys.stderr.write("mock cvent upstream listening on 127.0.0.1:%d (event %s)\n"
                     % (port, EVENT_CODE))
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
        sys.stderr.write("fixture written to %s/%s/ (+index.json)\n"
                         % (args.out, EVENT_CODE))
    if args.serve:
        serve(args.serve)
    if not args.out and not args.serve:
        ap.print_usage()
        sys.exit(2)


if __name__ == "__main__":
    main()
