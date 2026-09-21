#!/usr/bin/env bash
# test_contacts.sh — list contacts, inspect shape (small page only)
source "$(dirname "$0")/lib.sh"
require_env

log "GET /contacts?limit=5"
page=$(cvent_get /contacts 'limit=5')
printf '%s' "$page" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert "data" in d and "paging" in d, "missing data/paging"
p = d["paging"]
lim, tot = p["limit"], p["totalCount"]
print(f"contacts: limit={lim} total={tot}")
c = d["data"][0]
print("sample contact keys:", sorted(c.keys()))
print("sample contact:", {k: c.get(k) for k in ("id", "firstName", "lastName", "emailAddress") if k in c})
assert len(d["data"]) == 5, "limit=5 not honored"
' || die "contacts payload shape invalid"
pass "contacts list honors limit + paging"

pass "test_contacts.sh"
