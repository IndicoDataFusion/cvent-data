#!/usr/bin/env bash
# print_scopes.sh — fetch a fresh token and dump its scope claims
source "$(dirname "$0")/lib.sh"
require_env

tok=$(curl -sf --max-time 30 -X POST "$API_BASE/oauth2/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -H "Authorization: Basic $(printf '%s' "$CVENT_CLIENT_ID:$CVENT_CLIENT_SECRET" | base64 | tr -d '\n')" \
  --data "grant_type=client_credentials&client_id=$CVENT_CLIENT_ID" \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])') \
  || die "token request failed"

printf '%s' "$tok" | python3 -c '
import base64, json, sys
tok = sys.stdin.read().strip()
p = tok.split(".")[1]; p += "=" * (-len(p) % 4)
claims = json.loads(base64.urlsafe_b64decode(p))
scps = claims.get("scp", [])
print(f"scopes: {len(scps)}")
for s in sorted(scps):
    print("  ", s)
print("write scopes:", [s for s in scps if ":write" in s])
print("delete scopes:", [s for s in scps if ":delete" in s])
'
