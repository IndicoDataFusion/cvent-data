# Cvent REST API — endpoint map (verified 2026-09-21)

Full spec: `../openapi.json` (`make openapi`) (367 paths, 1332 schemas). **App scopes grew from
16 → 41** (admin added orders/transactions, sessions, speakers, players,
admission/fee/donation items, webcasts, videos, meeting-requests,
invitation-lists, weblinks, process-forms, session-enrollment/attendance, …).
That takes reachable operations from **37 → 94**.

Run:
- `tests/print_scopes.sh` — re-dump the live scope set
- `tests/probe_endpoints.sh <event-uuid>` — re-verify the read endpoints live
- `tests/pull_event.sh <code-or-uuid> [dest]` — pull a whole event
(default dest `tests/pulls/<code-or-uuid>`, gitignored).

## Payment status (NEW — `event/orders:read` + `event/transactions:read`)

| Endpoint | What you get |
|---|---|
| `GET /events/{id}/orders` | per-attendee order: **`amountOrdered`, `amountPaid`, `amountDue`**, `paymentMethod`, `invoiceNumber`, `cancelled`, `type` (Authorization / Online Charge / Online Refund / Offline Charge / Offline Refund) |
| `GET /events/{id}/orders/items` | line items: product, price, fee, qty, per-item amountOrdered/Paid/Due |
| `GET /events/{id}/transactions` | money events: `success` (bool), `paymentType`, `paymentMethod` (Visa/Amex/Paypal/Cash/Invoice/PO/…), amount, `nameOnCreditCard`, `date` |
| `GET /events/{id}/transactions/items` | which products each transaction covered |
| `GET /orders`, `/orders/{id}`, `/orders/{id}/items` | same, account-wide |
| `GET /transactions`, `/transactions/{id}/items` | same, account-wide |

Model: attendee → order(s) → order items; attendee → transactions. `amountDue > 0`
= unpaid balance. All returned 200 on the probe event.

## Pricing structure (NEW — `event/fee-items:read` + `event/admission-items:read`)

| Endpoint | Notes |
|---|---|
| `GET /events/{id}/fee-items` | fee items per admission item (name, amount, currency, product ref). |
| `POST /admission-items/filter` | admission/badge types; `{"filter":"event.id eq '<uuid>'"}`. |
| `GET /events/{id}/donation-items` | donation items |

Note: fee-items is **event-scoped** (`/events/{id}/fee-items`); admission-items is
top-level + filter (`POST /admission-items/filter`).

## Sessions / program (NEW — `event/sessions:read` etc.)

| Endpoint | Notes |
|---|---|
| `GET /sessions` (top-level) | all sessions, account-wide |
| `POST /sessions/filter` | `{"filter":"event.id eq '<uuid>'"}` |
| `GET /sessions/enrollment`, `POST /sessions/enrollment/filter` | who enrolled where |
| `GET /sessions/attendance` | session attendance |
| `GET /sessions/{id}` + `/speakers`, `/docs` | per-session detail |
| `GET /speakers` (top-level) | account-wide; `POST /speakers/filter` per-event |
| `GET /speaker-categories`, `/session-categories`, `/session-segments` | taxonomy |
| `GET /program-items/speakers` | program ↔ speaker mapping |
| `GET /webcasts/players` | (this is what `event/players:read` hits) |
| `GET /events/{id}/invitation-lists` | invitation lists (per event) |
| `GET /events/{id}/meeting-requests` | meeting-request config |
| `GET /events/{id}/event-travel/air-requests`, `.../hotel-requests` | travel requests |

## Account-scale reads (top-level lists)

| Endpoint | What it is |
|---|---|
| `GET /events` | all events (filter: `filter=code eq '<code>'`) |
| `POST /events/filter` | filterable list (body `{"filter":"status eq 'Active'"}`) |
| `GET /contacts` | all contacts (filterable via `POST /contacts/filter`) |
| `GET /attendees` | all attendee (event×contact) rows |
| `POST /attendees/filter` | `{"filter":"event.id eq '<uuid>'"}` |
| `GET /attendees/activities` | engagement log; `?filter=event.id eq '<uuid>'` |
| `GET /event-questions` | every registration/survey question across events |
| `GET /attendance-durations` | on-site dwell times per attendee |
| `GET /contact-types` | contact classification labels |
| `GET /contact-groups` | mailing/group lists (type, distributionListInfo) |
| `GET /custom-fields?filter=category eq 'Event'` | event-level custom field defs |
| `GET /custom-fields?filter=category eq 'Contact'` | contact-level custom field defs |
| `GET /webcasts/attendee-links` | (empty in testing) |
| `GET /attendees/activities/external/metadata` | (empty) |
| `GET /contacts/{id}/history` | contact change history (empty sample) |

## Per-event reads

| Endpoint | What it is |
|---|---|
| `GET /events/{id}` | full event object |
| `GET /events/{id}/registration-types` | ticket types (code, capacity, openForRegistration) |
| `GET /events/{id}/registration-paths` | registration flows |
| `GET /events/{id}/emails` | email campaigns (htmlBody, clickTrackingEnabled) |
| `GET /events/{id}/discounts` | discount codes |
| `GET /events/{id}/discounts/agenda-items` | per-session discounts (none) |

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

RFPs + suppliers + venues (13+16 paths), standard-surveys + responses,
housing/hotels, travel programs, bulk-jobs, SCIM, appointment scheduling,
budget/payments (venue budget side), ecommerce card-tokens, emails (send),
payments writes. (Sessions/speakers/orders/transactions moved INTO scope.)
