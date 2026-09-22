# Cvent REST API — endpoint map (verified 2026-09-21)

Full spec: `../openapi.json` (367 paths, 1332 schemas). Of the ~490 operations,
**37 are reachable with this app's 16 scopes** (15 read-list + 1 write). The
rest need scopes an admin hasn't granted (RFP, venues, sessions, surveys,
housing, travel, webcasts management, etc.).

Run `tests/probe_endpoints.sh [event-uuid]` to re-verify the read endpoints
live (defaults to CONF27 `4f1c2a9e-7b3d-4e8a-9c21-5d6e7f80a1b0`).

## Account-scale reads (top-level lists)

| Endpoint || What it is |
|---|---|
| `GET /events` || all events (filter: `filter=code eq '<code>'`) |
| `POST /events/filter` || filterable list (body `{"filter":"status eq 'Active'"}`) |
| `GET /contacts` || all contacts (filterable via `POST /contacts/filter`) |
| `GET /attendees` || all attendee (event×contact) rows |
| `POST /attendees/filter` || `{"filter":"event.id eq '<uuid>'"}` |
| `GET /attendees/activities` || engagement log; `?filter=event.id eq '<uuid>'` |
| `GET /event-questions` || every registration/survey question across events |
| `GET /attendance-durations` || on-site dwell times per attendee |
| `GET /contact-types` || contact classification labels |
| `GET /contact-groups` || mailing/group lists (type, distributionListInfo) |
| `GET /custom-fields?filter=category eq 'Event'` || event-level custom field defs |
| `GET /custom-fields?filter=category eq 'Contact'` || contact-level custom field defs |
| `GET /webcasts/attendee-links` || (empty in testing) |
| `GET /attendees/activities/external/metadata` || (empty) |
| `GET /contacts/{id}/history` || contact change history (empty sample) |

## Per-event reads (CONF27 `4f1c2a9e-…`)

| Endpoint || What it is |
|---|---|
| `GET /events/{id}` || full event object |
| `GET /events/{id}/registration-types` || ticket types (code, capacity, openForRegistration) |
| `GET /events/{id}/registration-paths` || registration flows |
| `GET /events/{id}/emails` || email campaigns (htmlBody, clickTrackingEnabled) |
| `GET /events/{id}/discounts` || discount codes |
| `GET /events/{id}/discounts/agenda-items` || per-session discounts (none) |

## Writes (only one scope)

- `POST /attendees`, `PUT /attendees/{id}`,
  `PUT /attendees/{id}/email-subscriptions`,
  `PUT /attendees/{id}/internal-information-questions` — create/update
  attendees. Nothing else is writable.
- `POST /events/{id}/check-in`, `DELETE /events/{id}/check-in/{attendeeId}` —
  check an attendee in/out (under `event/attendees:write`).

## Filter-language gotchas (learned by probing)

- Filter strings are OData-ish: `field eq 'value'`, `and`, `or`, `ne`.
- **String values must be single-quoted** in the expression
  (`status eq 'Active'`), not double-quoted.
- Dotted field names work on nested objects: `event.id eq '<uuid>'`.
- `/custom-fields` only supports `filter=category eq '<Event|Contact>'` —
  every other field is "Unsupported field … for filter".
- `/contacts/filter` and `/attendees/filter` are POST with a JSON body
  `{"filter":"…"}`; `/events` and `/events/filter` accept the filter as a
  query param or POST body respectively.
- Pagination: `paging.currentToken` → next request `?token=<currentToken>`.
  The server keeps minting a fresh token even on empty result sets, so stop
  on `collected ≥ totalCount`, not on token presence.

## Out of scope (would need new scopes from an admin)

RFPs + suppliers + venues (13+16 paths), sessions/program/speakers,
standard-surveys + responses, housing/hotels, travel, webcasts content,
bulk-jobs, SCIM, appointment scheduling, audience-segments (write),
card-tokens/orders/transactions (payments), emails (send).
