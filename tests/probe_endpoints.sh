#!/usr/bin/env bash
# probe_endpoints.sh — live-probe a list of in-scope endpoints, print status + shape
source "$(dirname "$0")/lib.sh"
require_env
EV="${1:?usage: probe_endpoints.sh <event-uuid>}"

probe() {
  local label="$1" path="$2"; shift 2
  local out code body summary
  out=$(curl -s -w '\n%{http_code}' --max-time 45 "$API_BASE$path" \
      -H "Authorization: Bearer $(cvent_token)" -H 'Accept: application/json' "$@")
  code=$(tail -n1 <<<"$out")
  body=$(head -n -1 <<<"$out")
  summary=$(printf '%s' "$body" | python3 -c '
import json,sys
raw = sys.stdin.read()
try:
    d = json.loads(raw)
except Exception:
    print("non-json: " + raw[:80]); raise SystemExit
if "paging" in d:
    tot = d["paging"].get("totalCount","?")
    sample = d.get("data") or []
    keys = sorted(sample[0].keys())[:8] if sample else []
    print(f"total={tot}  sample_keys={keys}")
elif "error" in d:
    print("ERROR: " + str(d["error"])[:140])
else:
    print("keys: " + str(sorted(d.keys())[:10]))
')
  printf '[%s] %-34s %s\n' "$code" "$label" "$summary"
}

echo "=== top-level list endpoints (in-scope) ==="
probe "GET /attendance-durations" "/attendance-durations?limit=3"
probe "GET /contact-groups" "/contact-groups?limit=3"
probe "GET /contact-types" "/contact-types?limit=3"
probe "GET /event-questions" "/event-questions?limit=3"
probe "GET /webcasts/attendee-links" "/webcasts/attendee-links?limit=3"
probe "GET /attendee-activities-metadata" "/attendees/activities/external/metadata?limit=3"
probe "POST /events/filter" "/events/filter" -X POST -H 'Content-Type: application/json' --data "{\"filter\":\"status eq 'Active'\"}"
# /custom-fields requires filter=category eq '<Event|Contact>' (no other fields supported)
probe "GET /custom-fields (Event)" "/custom-fields" -G --data-urlencode "filter=category eq 'Event'" --data-urlencode "limit=3"
probe "GET /custom-fields (Contact)" "/custom-fields" -G --data-urlencode "filter=category eq 'Contact'" --data-urlencode "limit=3"
# per-contact endpoints
CID=$(cvent_get /contacts 'limit=1' | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"][0]["id"])')
probe "GET /contacts/{id}/history" "/contacts/$CID/history"

echo "=== per-event endpoints ($EV) ==="
probe "GET /events/{id}/discounts" "/events/$EV/discounts"
probe "GET /events/{id}/emails" "/events/$EV/emails"
probe "GET /events/{id}/registration-paths" "/events/$EV/registration-paths"
probe "GET /events/{id}/registration-types" "/events/$EV/registration-types"
probe "GET /events/{id}/discounts/agenda-items" "/events/$EV/discounts/agenda-items"
