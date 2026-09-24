#!/usr/bin/env bash
# test_server.sh — server smoke suite: the real binary end-to-end, offline.
#
# The suite runs the REAL cvent-data-server against a LOCAL MOCK Cvent
# upstream (tests/make_fixture.py --serve): CVENT_API_BASE points at the
# mock, so every /api/cvent/* response is produced by the server's real
# code paths (event resolution, 12-resource fan-out, payments, attendee
# search, static/SPA serving). NO live network calls: the mock's access
# log is asserted to show only local paths.
#
# Self-contained and fast (<30 s): builds the binary, generates the
# fixture, and owns both server processes (trap-based cleanup).
source "$(dirname "$0")/lib.sh"

APP_PORT=8766
MOCK_PORT="${MOCK_PORT:-18990}"   # override if the default is busy
DEAD_PORT="${DEAD_PORT:-8767}"
DEAD_UPSTREAM="http://127.0.0.1:1"   # nothing listens here -> 502 path
BASE="http://127.0.0.1:$APP_PORT"

WORK=$(mktemp -d)
MOCK_PID=""; APP_PID=""; DEAD_PID=""
cleanup() {
  for p in "$MOCK_PID" "$APP_PID" "$DEAD_PID"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

port_free() {
python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
}

port_free "$APP_PORT"  || die "port $APP_PORT is busy — pick a free one (APP_PORT=... not supported here; kill the process and retry)"
port_free "$MOCK_PORT" || die "port $MOCK_PORT is busy"
port_free "$DEAD_PORT" || die "port $DEAD_PORT is busy"

# --- build + fixture ---------------------------------------------------------
log "building server binary"
(cd "$REPO_ROOT/server" && go build -o "$WORK/app" .) || die "go build failed"
[ -x "$WORK/app" ]; check $? "binary built"

log "generating fixture (dump layout) into $WORK/fixture"
python3 "$REPO_ROOT/tests/make_fixture.py" --out "$WORK/fixture" \
  || die "make_fixture.py --out failed"
[ -f "$WORK/fixture/index.json" ]; check $? "fixture index.json written"
[ -f "$WORK/fixture/syn-event/event.json" ]; check $? "fixture event.json written"

# Ground truth for the payments assertion: compute the expected totals from
# the fixture with the SAME rules as server/payments.go (non-cancelled
# orders sum to ordered/paid/due; refunded = transactions with success==true
# and paymentType containing "Refund"; 2-decimal rounding), then pin them
# to the hardcoded values so a fixture drift fails loudly here, not later.
EXPECTED_TOTALS=$(python3 - "$WORK/fixture" <<'PY'
import json, sys
fx = sys.argv[1]
orders = json.load(open(f"{fx}/syn-event/orders.json"))
txns = json.load(open(f"{fx}/syn-event/transactions.json"))
ordered = paid = due = refunded = 0.0
for o in orders:
    if o.get("cancelled"):
        continue
    ordered += o.get("amountOrdered", 0)
    paid += o.get("amountPaid", 0)
    due += o.get("amountDue", 0)
for t in txns:
    if t.get("success") and "Refund" in t.get("paymentType", ""):
        refunded += t.get("amount", 0)
# math.Round (half away from zero) == round() for these exact .00/.50/.25 values
print(json.dumps({"ordered": round(ordered, 2), "paid": round(paid, 2),
                  "due": round(due, 2), "refunded": round(refunded, 2)}))
PY
) || die "computing expected totals failed"
# Fixture ground truth (see tests/make_fixture.py header for the fixture):
#   ordered  = 100.00 (o1 paid) + 200.00 (o2 partial) + 300.00 (o3 unpaid) = 600.00
#   paid     = 100.00 + 50.00 + 0.00 = 150.00
#   due      = 0.00 + 150.00 + 300.00 = 450.00
#   refunded = 25.00 (txn-3, success Refund — excluded from paid/due)
[ "$EXPECTED_TOTALS" = '{"ordered": 600.0, "paid": 150.0, "due": 450.0, "refunded": 25.0}' ] \
  || die "fixture ground truth drift: $EXPECTED_TOTALS"
pass "expected totals computed from fixture: $EXPECTED_TOTALS"

# --- start mock + app --------------------------------------------------------
log "starting mock Cvent upstream on 127.0.0.1:$MOCK_PORT"
python3 "$REPO_ROOT/tests/make_fixture.py" --serve "$MOCK_PORT" \
  2> "$WORK/mock.log" &
MOCK_PID=$!
wait_http() { # <url> <seconds>
  local url="$1" t="$2" i=0
  while [ "$i" -lt $((t * 20)) ]; do
    if curl -sf -o /dev/null --max-time 2 "$url"; then return 0; fi
    sleep 0.05; i=$((i + 1))
  done
  return 1
}
wait_http "http://127.0.0.1:$MOCK_PORT/events?limit=1" 5 \
  || die "mock upstream did not come up (see $WORK/mock.log)"
pass "mock upstream ready"

log "starting app on :$APP_PORT (event=syn-event, data=$WORK/fixture, web=web)"
# OS env beats the repo .env (loadCventEnv prefers os.Getenv), so the real
# creds in .env are never used; the mock accepts any Basic auth anyway.
(
  cd "$REPO_ROOT"
  exec env CVENT_API_BASE="http://127.0.0.1:$MOCK_PORT" \
    CVENT_CLIENT_ID=test CVENT_CLIENT_SECRET=*** \
    "$WORK/app" --addr ":$APP_PORT" --event syn-event \
    --data "$WORK/fixture" --web web
) 2> "$WORK/app.log" &
APP_PID=$!
wait_http "$BASE/api/health" 5 || die "app did not come up (see $WORK/app.log)"
pass "app ready"

# --- 1. health ---------------------------------------------------------------
# Note on the "[ cond ]; check $?" idiom (shared with test_auth.sh & co):
# under lib.sh's set -e a FAILED [ ] exits the script with 1 at that line
# before check ever runs — run_all.sh counts it as a failed test. On a pass,
# check $1=0 prints PASS. Same contract, same behavior as the other scripts.
code=$(curl -s -o "$WORK/health.json" -w '%{http_code}' "$BASE/api/health")
[ "$code" = "200" ]; check $? "/api/health -> 200 (got $code)"
python3 - "$WORK/health.json" <<'PY' && pass "health ok=true" || die "health ok != true"
import json, sys
d = json.load(open(sys.argv[1]))
assert d.get("ok") is True, f"ok != true: {d}"
PY

# --- 2. bundle: fan-out end-to-end against the mock ---------------------------
code=$(curl -s -o "$WORK/bundle.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event")
[ "$code" = "200" ]; check $? "/api/cvent/events/syn-event -> 200 (got $code)"
python3 - "$WORK/bundle.json" <<'PY' && pass "bundle: 12 count keys, attendees=5, title ok" \
  || die "bundle payload wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["code"] == "syn-event", d.get("code")
assert d["event"]["title"] == "Synthetic Test Event", d["event"].get("title")
assert d["event"]["id"] == "00000000-1111-4222-8333-000000000001"
c = d["counts"]
expected_keys = {"attendees", "activities", "orders", "orderItems",
                 "transactions", "transactionItems", "feeItems",
                 "admissionItems", "registrationTypes", "discounts",
                 "sessions", "speakers"}
assert expected_keys <= set(c), f"missing count keys: {expected_keys - set(c)}"
assert len(expected_keys & set(c)) == 12, f"expected exactly 12 count keys, got {len(expected_keys & set(c))}"
assert c["attendees"] == 5, f"counts.attendees != 5: {c['attendees']}"
# non-empty fan-out resources the fixture populates
for k, n in [("orders", 3), ("transactions", 3), ("feeItems", 2),
             ("admissionItems", 2), ("registrationTypes", 2)]:
    assert c[k] == n, f"counts[{k}] != {n}: {c[k]}"
PY

# --- 3. payments ---------------------------------------------------------------
code=$(curl -s -o "$WORK/pay.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event/payments")
[ "$code" = "200" ]; check $? "/api/cvent/events/syn-event/payments -> 200 (got $code)"
python3 - "$WORK/pay.json" "$EXPECTED_TOTALS" <<'PY' && pass "payments: totals + rows exact" \
  || die "payments payload wrong"
import json, sys
api = json.load(open(sys.argv[1]))
expected = json.loads(sys.argv[2])
# Totals EXACTLY the fixture ground truth (arithmetic in the comment above
# the EXPECTED_TOTALS pin): ordered 600.00 / paid 150.00 / due 450.00 /
# refunded 25.00. paid==150.0 and due==450.0 prove the 25.00 refund is NOT
# counted in paid/due — those sum order amounts only, never transactions.
assert api["totals"] == expected, f"totals {api['totals']} != {expected}"
assert len(api["orders"]) == 3, f"expected 3 order rows, got {len(api['orders'])}"
assert api.get("cancelled") == [], "no cancelled orders in fixture"
# Sorted by amountDue desc (payments.go): o3 300 unpaid, o2 150 partial, o1 0 paid.
rows = [(r["amountDue"], r["status"], r["attendee"], r["invoice"]) for r in api["orders"]]
assert rows == [
    (300.0, "unpaid", "Dan Okafor", "INV-1003"),    # id via order.contact.id
    (150.0, "partial", "Carol Jones", "INV-1002"),  # id via order.attendeeId
    (0.0, "paid", "Alice Zetar", "INV-1001"),       # id via order.attendee.id
], f"order rows wrong: {rows}"
PY

# --- 4./5. attendee search -----------------------------------------------------
code=$(curl -s -o "$WORK/att_zeta.json" -w '%{http_code}' \
  "$BASE/api/cvent/events/syn-event/attendees?q=zeta")
[ "$code" = "200" ]; check $? "/api/cvent/events/syn-event/attendees?q=zeta -> 200 (got $code)"
python3 - "$WORK/att_zeta.json" <<'PY' && pass "q=zeta -> total 2, items 2" || die "q=zeta wrong"
import json, sys
d = json.load(open(sys.argv[1]))
# "zeta" matches EXACTLY 2 attendees: Alice Zetar (name.lastName) and Bob
# Smith (contact.email zeta@example.org) — the haystack covers name parts +
# email + confirmationNumber, case-insensitive.
assert d["total"] == 2, f"total != 2: {d['total']}"
assert len(d["items"]) == 2, f"items != 2"
ids = sorted(i["id"] for i in d["items"])
assert ids == ["att-001", "att-002"], ids
PY

code=$(curl -s -o "$WORK/att_nomatch.json" -w '%{http_code}' \
  "$BASE/api/cvent/events/syn-event/attendees?q=zzz-no-match")
[ "$code" = "200" ]; check $? "attendees?q=zzz-no-match -> 200 (got $code)"
python3 - "$WORK/att_nomatch.json" <<'PY' && pass "q=zzz-no-match -> total 0, items []" \
  || die "no-match search wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["total"] == 0, f"total != 0: {d}"
assert d["items"] == [], f"items != []: {d['items']}"
PY

# --- 6. repull-status -----------------------------------------------------------
code=$(curl -s -o "$WORK/repull.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event/repull-status")
[ "$code" = "200" ]; check $? "repull-status -> 200 (got $code)"
python3 - "$WORK/repull.json" <<'PY' && pass "repull-status: running=false, pulledAt set" \
  || die "repull-status wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["running"] is False, d
assert d["pulledAt"] is not None and d["pulledAt"] != "", "pulledAt null (bundle never fetched?)"
PY

# --- 6b. catalog: /api/cvent/events lists both fixture events --------------------
code=$(curl -s -o "$WORK/catalog.json" -w '%{http_code}' "$BASE/api/cvent/events")
[ "$code" = "200" ]; check $? "/api/cvent/events -> 200 (got $code)"
python3 - "$WORK/catalog.json" <<'PY' && pass "catalog: 2 events, default=syn-event" \
  || die "catalog payload wrong"
import json, sys
d = json.load(open(sys.argv[1]))
codes = sorted(e["code"] for e in d["events"])
assert codes == ["syn-event", "syn-event-2"], f"catalog codes {codes}"
assert d["default"] == "syn-event", d
PY

# --- 6c. second event: event-scoped routes serve THAT event's data ---------------
# Guards against the single-event assumption leaking back: syn-event-2 has
# 1 attendee / 1 paid order (400.00) — distinct from syn-event's 5 / 3.
code=$(curl -s -o "$WORK/bundle2.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event-2")
[ "$code" = "200" ]; check $? "/api/cvent/events/syn-event-2 -> 200 (got $code)"
python3 - "$WORK/bundle2.json" <<'PY' && pass "syn-event-2 bundle: title + counts distinct" \
  || die "syn-event-2 bundle wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["code"] == "syn-event-2", d.get("code")
assert d["event"]["title"] == "Synthetic Test Event 2", d["event"].get("title")
c = d["counts"]
assert c["attendees"] == 1 and c["orders"] == 1, f"counts {c}"
PY
code=$(curl -s -o "$WORK/pay2.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event-2/payments")
[ "$code" = "200" ]; check $? "syn-event-2 payments -> 200 (got $code)"
python3 - "$WORK/pay2.json" <<'PY' && pass "syn-event-2 payments: 1 order, ordered 400.00" \
  || die "syn-event-2 payments wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["totals"] == {"ordered": 400.0, "paid": 400.0, "due": 0.0, "refunded": 0.0}, d["totals"]
assert len(d["orders"]) == 1 and d["orders"][0]["attendee"] == "Frank Grant", d["orders"]
PY
code=$(curl -s -o "$WORK/att2.json" -w '%{http_code}' "$BASE/api/cvent/events/syn-event-2/attendees?q=frank")
[ "$code" = "200" ]; check $? "syn-event-2 attendees?q=frank -> 200 (got $code)"
python3 - "$WORK/att2.json" <<'PY' && pass "syn-event-2 q=frank -> total 1 (Frank Grant)" \
  || die "syn-event-2 attendee search wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["total"] == 1 and d["items"][0]["id"] == "att-201", d
PY

# --- 7. unknown /api source ------------------------------------------------------
code=$(curl -s -o "$WORK/nope.json" -w '%{http_code}' "$BASE/api/nope/x")
[ "$code" = "404" ]; check $? "/api/nope/x -> 404 (got $code)"
grep -q 'unknown source' "$WORK/nope.json" \
  && pass "404 body is the unknown-source JSON error" \
  || die "404 body missing 'unknown source': $(cat "$WORK/nope.json")"

# --- 8. SPA fallback ----------------------------------------------------------------
code=$(curl -s -o "$WORK/spa.html" -w '%{http_code}' "$BASE/x/y")
ctype=$(curl -s -o /dev/null -w '%{content_type}' "$BASE/x/y")
[ "$code" = "200" ]; check $? "/x/y (no file) -> 200 SPA fallback (got $code)"
case "$ctype" in text/html*) : ;; *) die "content-type $ctype, expected text/html" ;; esac
pass "SPA fallback content-type is text/html"
grep -q 'Cvent Data' "$WORK/spa.html" \
  && pass "SPA fallback body is index.html (marker found)" \
  || die "SPA fallback body missing index.html marker"

# --- 9./10. /data — variant B: --data points at the fixture dump dir ---------------
# So /data/index.json is a REAL 200 (JSON array, one entry: syn-event) while
# /data/missing.json still proves the Task-7 follow-up behavior: a missing
# data file is a real 404 JSON, never an HTML SPA fallback.
code=$(curl -s -o "$WORK/data_missing.json" -w '%{http_code}' "$BASE/data/missing.json")
[ "$code" = "404" ]; check $? "/data/missing.json -> 404 (got $code)"
grep -q 'not found' "$WORK/data_missing.json" \
  && pass "/data/missing.json is a JSON 404 (not HTML)" \
  || die "/data/missing.json body not JSON error: $(cat "$WORK/data_missing.json")"

code=$(curl -s -o "$WORK/data_index.json" -w '%{http_code}' "$BASE/data/index.json")
[ "$code" = "200" ]; check $? "/data/index.json -> 200 (got $code)"
python3 - "$WORK/data_index.json" <<'PY' && pass "/data/index.json is the fixture dump index (2 events)" \
  || die "/data/index.json wrong"
import json, sys
d = json.load(open(sys.argv[1]))
assert isinstance(d, list) and len(d) == 2, f"expected 2-entry array: {d}"
assert sorted(e["code"] for e in d) == ["syn-event", "syn-event-2"], d
PY

# --- offline proof: the mock log contains only local paths -------------------------
grep -q '^GET /events?' "$WORK/mock.log" \
  && pass "mock log shows the code->uuid resolution hit the MOCK" \
  || die "mock log missing GET /events?filter=... (resolution never reached the mock?)"
if grep -q 'api-platform\.cvent\.com' "$WORK/mock.log"; then
  die "mock log references the real Cvent host — network leak"
fi
pass "mock log has zero references to api-platform.cvent.com (offline)"

# --- 11. error path: app pointed at a DEAD mock -> 502, bounded -------------------
log "starting second app on :$DEAD_PORT with CVENT_API_BASE=$DEAD_UPSTREAM"
(
  cd "$REPO_ROOT"
  exec env CVENT_API_BASE="$DEAD_UPSTREAM" \
    CVENT_CLIENT_ID=test CVENT_CLIENT_SECRET=*** \
    "$WORK/app" --addr ":$DEAD_PORT" --event syn-event \
    --data "$WORK/fixture" --web web
) 2> "$WORK/app_dead.log" &
DEAD_PID=$!
wait_http "http://127.0.0.1:$DEAD_PORT/api/health" 5 \
  || die "dead-upstream app did not come up (see $WORK/app_dead.log)"
# The 20 s fan-out timeout (event.go) bounds this; allow 25 s. Connection
# refusals fail faster, but the bound must hold even for a black-holing host.
code=$(curl -s -o "$WORK/dead_event.json" -w '%{http_code}' --max-time 25 \
  "http://127.0.0.1:$DEAD_PORT/api/cvent/events/syn-event")
[ "$code" = "502" ]; check $? "dead upstream: /api/cvent/events/syn-event -> 502 within 25 s (got $code)"
grep -q '"error"' "$WORK/dead_event.json" \
  && pass "dead upstream: 502 body is JSON with an error message" \
  || die "dead upstream 502 body not JSON: $(cat "$WORK/dead_event.json")"

pass "test_server.sh"
