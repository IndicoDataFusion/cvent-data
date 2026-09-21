# cvent-data

Client, tests, and reference material for the Cvent Platform REST API.

- `openapi.json` — full official OpenAPI 3.0.2 spec (367 endpoints), pulled
  from the Cvent developer portal. Use it as the source of truth for
  endpoints, schemas, and scopes.
- `tests/` — bash smoke tests against the live API (see `tests/README.md`).
- `.env` (gitignored) — `CVENT_CLIENT_ID` / `CVENT_CLIENT_SECRET`.

## Quick auth

```bash
set -a; . ./.env; set +a
TOKEN=*** -s -X POST "https://api-platform.cvent.com/ea/oauth2/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -H "Authorization: Basic $(printf '%s' "$CVENT_CLIENT_ID:$CVENT_CLIENT_SECRET" | base64 | tr -d '\n')" \
  -d "grant_type=client_credentials&client_id=$CVENT_CLIENT_ID" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')

curl -s "https://api-platform.cvent.com/ea/events?limit=5" \
  -H "Authorization: Bearer $TOKEN"
```
