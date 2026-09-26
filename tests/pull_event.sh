#!/usr/bin/env bash
# pull_event.sh — pull one event's full data into a local folder.
#
# Usage: tests/pull_event.sh <code-or-uuid> [dest-dir]
#   <code-or-uuid>  short event code (e.g. ABC123XYZ) or UUID
#   [dest-dir]      output directory (default: tests/pulls/<code-or-uuid>, gitignored)
#
# Writes:
#   <dest>/event.json             full event object
#   <dest>/attendees.json         all attendee rows (paginated, merged)
#   <dest>/activities.json        all attendee activities (paginated, merged)
#   <dest>/registration-types.json
#   <dest>/registration-paths.json
#   <dest>/discounts.json
#   <dest>/emails.json            email campaigns (incl. htmlBody)
#   <dest>/event-questions.json   registration/survey questions
#   <dest>/orders.json            payment status: amountOrdered/Paid/Due per attendee
#   <dest>/order-items.json       order line items
#   <dest>/transactions.json      payment events (success flag, method, type)
#   <dest>/transaction-items.json
#   <dest>/fee-items.json         pricing (fee items per admission item)
#   <dest>/admission-items.json   admission/badge types
#   <dest>/_pages/                raw per-page responses (kept for debugging)
#
# Each merged file is {"source": …, "totalCount": N, "data": […]}.
source "$(dirname "$0")/lib.sh"
require_env

ref="${1:-}"
[ -n "$ref" ] || die "usage: pull_event.sh <code-or-uuid> [dest-dir]"

# --- resolve to UUID ---------------------------------------------------------
if [[ "$ref" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; then
  event_id="$ref"
  log "using UUID directly: $event_id"
else
  log "resolving code '$ref' to UUID"
  event_id=$(cvent_get /events "filter=code eq '$ref'" | python3 -c '
import json, sys
code = sys.argv[1]
d = json.load(sys.stdin)
n = d["paging"]["totalCount"]
if n == 0:
    sys.exit(f"no event found with code {code}")
if n > 1:
    sys.exit(f"code matches {n} events — use a UUID")
print(d["data"][0]["id"])
' "$ref") || die "could not resolve '$ref'"
fi
[ -n "$event_id" ]; check $? "event id = $event_id"

dest="${2:-$(dirname "$0")/pulls/$ref}"
mkdir -p "$dest/_pages"
pages="$dest/_pages"
log "output dir: $dest"

# --- event object -------------------------------------------------------------
log "GET /events/$event_id"
cvent_get "/events/$event_id" > "$dest/event.json"
[ -s "$dest/event.json" ]; check $? "event.json written"
python3 -c '
import json, sys
e = json.load(open(sys.argv[1]))
print("  title:", e.get("title"))
print("  status:", e.get("status"), "/", e.get("eventStatus"), "|", e.get("start","?")[:10], "..", e.get("end","?")[:10])
' "$dest/event.json" || die "event.json invalid"

# --- generic paginated pull ----------------------------------------------------
# pull_list <label> <url-without-token> [extra curl args...]
#   Appends each page's data[] rows to $pages/<label>.jsonl (one row per line),
#   follows paging.currentToken until exhausted.
pull_list() {
  local label="$1" url="$2"; shift 2
  local out="$pages/$label.jsonl" tok="" n=0 i=0 total=""
  : > "$out"
  while :; do
    i=$((i+1))
    local u="$url"
    [ -n "$tok" ] && u="$url?token=$tok"
    curl -sf --max-time 120 "$u" \
      -H "Authorization: Bearer $(cvent_token)" -H 'Accept: application/json' \
      "$@" > "$pages/$label.page$i.json" || die "$label: page $i fetch failed"
    local rows tok_new
    rows=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["data"]))' "$pages/$label.page$i.json")
    tok_new=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["paging"].get("currentToken") or "")' "$pages/$label.page$i.json")
    [ -z "$total" ] && total=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["paging"].get("totalCount", ""))' "$pages/$label.page$i.json")
    n=$((n+rows))
    python3 -c '
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
out = open(sys.argv[2], "a")
for row in d["data"]:
    out.write(json.dumps(row) + "\n")
' "$pages/$label.page$i.json" "$out"
    log "  $label: page $i → $rows rows (collected $n/${total:-?})"
    # Termination: no next token, or everything collected (the server keeps
    # minting fresh tokens even for empty result sets, so token presence
    # alone is not a reliable stop signal).
    if [ -z "$tok_new" ]; then break; fi
    if [ -n "$total" ] && [ "$n" -ge "$total" ]; then break; fi
    if [ "$i" -ge 10000 ]; then die "$label: hit 10000-page safety cap"; fi
    tok="$tok_new"
  done
  # merge jsonl -> {"source","totalCount","data"}
  python3 -c '
import json, sys
label, pages_dir, src, out = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
rows = [json.loads(l) for l in open(f"{pages_dir}/{label}.jsonl")]
json.dump({"source": src, "totalCount": len(rows), "data": rows}, open(out, "w"), indent=1)
print(f"  {label}: merged {len(rows)} rows -> {out}")
' "$label" "$pages" "$label" "$dest/$label.json" || die "$label: merge failed"
}

# --- attendees -----------------------------------------------------------------
log "pulling attendees (POST /attendees/filter)"
pull_list attendees "$API_BASE/attendees/filter" \
  -X POST -H 'Content-Type: application/json' \
  --data "{\"filter\":\"event.id eq '$event_id'\"}"

# --- activities -----------------------------------------------------------------
log "pulling activities (GET /attendees/activities)"
pull_list activities "$API_BASE/attendees/activities" \
  -G --data-urlencode "filter=event.id eq '$event_id'"

# --- registration config ----------------------------------------------------------
log "pulling registration-types"
pull_list registration-types "$API_BASE/events/$event_id/registration-types"

log "pulling registration-paths"
pull_list registration-paths "$API_BASE/events/$event_id/registration-paths"

log "pulling discounts"
pull_list discounts "$API_BASE/events/$event_id/discounts"

# --- emails (campaigns, incl. htmlBody) -------------------------------------------
log "pulling emails"
pull_list emails "$API_BASE/events/$event_id/emails"

# --- event questions (registration/survey form) ------------------------------------
log "pulling event-questions"
pull_list event-questions "$API_BASE/event-questions" \
  -G --data-urlencode "filter=event.id eq '$event_id'"

# --- payment status (needs event/orders:read + event/transactions:read) --------------
log "pulling orders"
pull_list orders "$API_BASE/events/$event_id/orders"

log "pulling order items"
pull_list order-items "$API_BASE/events/$event_id/orders/items"

log "pulling transactions"
pull_list transactions "$API_BASE/events/$event_id/transactions"

log "pulling transaction items"
pull_list transaction-items "$API_BASE/events/$event_id/transactions/items"

# --- pricing structure (needs event/fee-items:read + event/admission-items:read) -----
log "pulling fee-items"
pull_list fee-items "$API_BASE/events/$event_id/fee-items"

log "pulling admission-items"
pull_list admission-items "$API_BASE/admission-items/filter" \
  -X POST -H 'Content-Type: application/json' \
  --data "{\"filter\":\"event.id eq '$event_id'\"}"

pass "pull_event.sh — data in $dest (raw pages in $dest/_pages)"
