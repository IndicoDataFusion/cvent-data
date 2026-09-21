#!/usr/bin/env bash
# test_auth.sh — OAuth2 client-credentials token flow + JWT inspection
source "$(dirname "$0")/lib.sh"
require_env

log "requesting token from $API_BASE/oauth2/token"
resp=$(curl -s --max-time 30 -w '\n%{http_code}' -X POST "$API_BASE/oauth2/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -H "Authorization: Basic $(printf '%s' "$CVENT_CLIENT_ID:$CVENT_CLIENT_SECRET" | base64 | tr -d '\n')" \
  --data "grant_type=client_credentials&client_id=$CVENT_CLIENT_ID")
code=$(tail -n1 <<<"$resp")
body=$(head -n -1 <<<"$resp")

[ "$code" = "200" ]; check $? "HTTP 200 from token endpoint"
printf '%s' "$body" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("access_token")' \
  && pass "response contains access_token" || die "no access_token in response"

# Inspect JWT claims (stdin = piped body; code passed via -c so stdin stays free)
printf '%s' "$body" | python3 -c '
import base64, json, sys
tok = json.load(sys.stdin)["access_token"]
p = tok.split(".")[1]; p += "=" * (-len(p) % 4)
claims = json.loads(base64.urlsafe_b64decode(p))
print("scopes (%d):" % len(claims.get("scp", [])))
for s in sorted(claims.get("scp", [])): print("  -", s)
w = [s for s in claims.get("scp", []) if ":write" in s]
print("write scopes:", w if w else "none")
' || die "JWT decode failed"

# Bad credentials must be rejected
bad=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -X POST "$API_BASE/oauth2/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -H "Authorization: Basic $(printf '%s' 'wrong:wrong' | base64 | tr -d '\n')" \
  -d 'grant_type=client_credentials&client_id=wrong')
[ "$bad" = "401" ] || [ "$bad" = "400" ]; check $? "bad credentials rejected (got $bad)"

pass "test_auth.sh"
