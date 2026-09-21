#!/usr/bin/env bash
# test_attendees.sh — attendees are a TOP-LEVEL resource (not /events/{id}/attendees)
#   GET  /attendees            -> account-wide list
#   POST /attendees/filter     -> filtered list, OData-ish: event.id eq '<uuid>'
#   GET  /attendees/activities -> engagement events (registrations, check-ins, ...)
source "$(dirname "$0")/lib.sh"
require_env

log "finding first event id"
event_id=$(cvent_get /events 'limit=1' | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"][0]["id"])')
[ -n "$event_id" ]; check $? "got event id ($event_id)"

log "GET /attendees?limit=3 (account-wide)"
page=$(cvent_get /attendees 'limit=3')
printf '%s' "$page" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert "data" in d and "paging" in d, "missing data/paging"
p = d["paging"]
lim, tot = p["limit"], p["totalCount"]
print(f"attendees: limit={lim} total={tot}")
a = d["data"][0]
for k in ("id", "event", "contact", "checkedIn", "confirmationNumber"):
    assert k in a, f"attendee missing field {k}"
print("attendee event:", {k: a["event"].get(k) for k in ("id", "name") if k in a.get("event", {})})
' || die "attendees payload shape invalid"
pass "top-level /attendees list works"

log "POST /attendees/filter  event.id eq '$event_id'"
filtered=$(cvent_get /attendees/filter 'filter=' 2>/dev/null || true)
# filter must go in the request body, so call curl directly with the lib token
filtered=$(curl -sf --max-time 60 -X POST "$API_BASE/attendees/filter" \
  -H "Authorization: Bearer $(cvent_token)" -H 'Content-Type: application/json' -H 'Accept: application/json' \
  --data "{\"filter\":\"event.id eq '$event_id'\"}")
n=$(printf '%s' "$filtered" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["paging"]["totalCount"])')
[ -n "$n" ]; check $? "filter returned $n attendees for the event"
pass "POST /attendees/filter with event.id works"

log "GET /attendees/activities?filter=event.id eq '$event_id'"
acts=$(curl -sf --max-time 60 -G "$API_BASE/attendees/activities" \
  -H "Authorization: Bearer $(cvent_token)" -H 'Accept: application/json' \
  --data-urlencode "filter=event.id eq '$event_id'")
printf '%s' "$acts" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert "data" in d and "paging" in d, "missing data/paging"
tot = d["paging"]["totalCount"]
print(f"activities for event: {tot}")
if d["data"]:
    a = d["data"][0]
    print("sample activity keys:", sorted(a.keys()))
    print("type:", a.get("type"), "| name:", a.get("name"))
' || die "activities payload invalid"
pass "attendee activities retrievable"

# Scope sanity: token must carry attendee read + write
scp=$(cvent_token | python3 -c '
import base64, json, sys
tok = sys.stdin.read().strip()
p = tok.split(".")[1]; p += "=" * (-len(p) % 4)
print(" ".join(json.loads(base64.urlsafe_b64decode(p)).get("scp", [])))
')
case " $scp " in
  *"event/attendees:read "*) : ;;
  *) die "missing event/attendees:read scope" ;;
esac
pass "token carries event/attendees:read"

pass "test_attendees.sh"
