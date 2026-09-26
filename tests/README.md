# Cvent REST API — test scripts

Shell-based smoke tests against the live Cvent Platform REST API.
No framework — bash + curl + python3 (stdlib only) for JSON checks.

## Run

```bash
tests/run_all.sh          # everything
tests/test_auth.sh        # OAuth2 token flow + JWT scope dump
tests/test_events.sh      # /events listing + cursor pagination
tests/test_contacts.sh    # /contacts listing + limit
tests/test_attendees.sh   # /attendees + /attendees/filter + activities
tests/probe_endpoints.sh <event-uuid>  # live-probe all in-scope read endpoints
tests/pull_event.sh <code-or-uuid> [dest]   # full pull of one event
```

Endpoint-by-endpoint findings (totals, shapes, filter gotchas): `ENDPOINTS.md`.

Requires `.env` in the repo root with `CVENT_CLIENT_ID` / `CVENT_CLIENT_SECRET`
(gitignored). Override the base URL with `CVENT_API_BASE` (EU:
`https://api-platform-eur.cvent.com/ea`).

## Verified API facts (2026-09-21, NA region)

- **Base**: `https://api-platform.cvent.com/ea`
- **Auth**: OAuth2 client credentials → `POST /oauth2/token`, Basic auth of
  base64(`id:secret`) (NO trailing newline), 60-min bearer token.
  Token is a JWT; decode the payload to see the app's `scp` (scopes) live.
- **Pagination**: `paging: {limit, totalCount, currentToken, _links}` —
  pass `token=<currentToken>` as query param; absent `currentToken` = last page.
  `limit` query param works on list endpoints.
- **OpenAPI spec**: full 3.0.2 spec (367 paths); `make openapi` saves it to
  `../openapi.json` (gitignored — it is Cvent's document).
  Fetched via `POST https://developers.cvent.com/api/graphql`
  body `{"query":"{ getPublicSpec }"}` (unauthenticated).

### Endpoint corrections found by probing

- Attendees are **top-level**: `GET /attendees` (not `/events/{id}/attendees`,
  which 404s) and account-wide — expect millions of rows on a large account.
- Per-event filtering: `POST /attendees/filter` with JSON body
  `{"filter":"event.id eq '<event-uuid>'"}`. Filter syntax is OData-ish;
  `event_id` (flat) is rejected — dotted field names are required.
- Activities: `GET /attendees/activities?filter=event.id eq '<uuid>'`
  (filter as **query param** here, not body).

### App scopes (from JWT `scp` claim)

Read: events, contacts, contact-types, contact-groups, attendees,
attendee-activities(+metadata), attendee-links, registration-types,
registration-paths, event-discounts, custom-fields, event-emails,
event-email-status, attendance-durations.
Write: **attendees only**.
No RFP/supplier/sessions/speakers/survey scopes — ask an admin to add them
in Developer Portal → Applications → Scopes to access those.
