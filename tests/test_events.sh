#!/usr/bin/env bash
# test_events.sh — list events, inspect shape, verify pagination fields
source "$(dirname "$0")/lib.sh"
require_env

log "GET /events (first page)"
page=$(cvent_get /events)
printf '%s' "$page" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert "data" in d and "paging" in d, "missing data/paging"
p = d["paging"]
for k in ("limit", "totalCount", "currentToken"):
    assert k in p, f"paging missing {k}"
lim, tot = p["limit"], p["totalCount"]
print(f"events: limit={lim} total={tot}")
e = d["data"][0]
print("sample event keys:", sorted(e.keys()))
print("sample event:", {k: e.get(k) for k in ("id", "name", "startDate") if k in e})
' || die "events payload shape invalid"
pass "event list returns data + paging"

# Walk one more page via currentToken to prove pagination works
tok=$(printf '%s' "$page" | python3 -c 'import json,sys; print(json.load(sys.stdin)["paging"].get("currentToken",""))')
[ -n "$tok" ]; check $? "paging.currentToken present"
page2=$(cvent_get /events "token=$tok")
printf '%s' "$page2" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["paging"].get("currentToken")' \
  && pass "second page fetch via token works" || die "second page invalid"

pass "test_events.sh"
