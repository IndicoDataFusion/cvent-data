# Changelog

## v0.2.1 — 2026-10-17

- `FetchDiscounts` and `DiscountsFrom`: the event's discount codes as typed
  `Discount` values (code, name, method and value, active flag, validity
  dates, level and audience, and the redemption count `CapacityUsed` of
  `CapacityTotal`, -1 = unlimited) from one `GET /events/{id}/discounts`,
  without the full event bundle.

## v0.2.0 — 2026-10-15

- `cvent.Registrant` is the one attendee record: the attendee search
  (`GET /api/cvent/events/{code}/attendees`) returns it as `items` (the
  `questions` map is gone: answers carry their question text), the dump
  writes it to `<code>/registrants.json`, and the Attendees view renders it.
  The browser no longer joins guests and orders itself.
- `BuildRegistrants` (one builder for every path) and
  `RegistrantsFromBundle`; `Registrant` gains the remaining contact and
  registration fields (prefix, CC email, work/home phone, the other
  address, admission item, registration path, registered/cancelled dates,
  check-in time) and `RegisteredBy` on guests. `GuestRef` is now
  `PersonRef` and carries the confirmation number.
- The event bundle reads each successful charge's billing address
  (`EventBundle.BillingAddresses`), so the sheet shows it. This costs one
  `GET /transactions/{id}` per charge on every bundle fetch.
- Attendee sheet shows everything the registrant entered: every contact
  field (CC email, work/home phone, both addresses with line 2 and region,
  prefix, pronouns, website), a Registration block (type, admission item,
  path, registered date, note), and the order: each selected item (guest
  fees marked), ordered/paid/due, invoice, payment method and discount.
- Guests and their primary are linked: a guest's sheet shows Registered by
  (and that the fee is on the primary's order), the primary's lists their
  Guests, and each name opens that person's sheet. The join runs in the
  browser over the event bundle, so live and static mode match.
- `Registrant` carries the full registration form: `Title`, `MobilePhone`,
  `EmergencyContactName` / `EmergencyContactPhone` and `VisaLetter` (from
  contact custom fields), `Address`, `CustomFields`, and `Answers` with
  their question text (`LabelAnswers`).
- Guests: `Guest` and `PrimaryID` on the guest, `Guests` on the primary.
- `WithPayments` also fills `AttendeePayment.Items`, the selected order
  lines (`OrderItemsByAttendee`); a guest's fee has `ForGuest`.
- `RegistrantsOptions.WithBillingAddress` fills `BillingAddress` from the
  transaction details (`Client.BillingAddresses`); `InvoiceAddress()`
  prefers it over the contact address.
- `FetchRegistrants` now always fetches the event questions, also when
  `RefQuestionID` is pinned, to label the answers.
- Overview: a Discounts section lists each discount code with its value,
  Cvent's used / total capacity and a status (Active, Used up, Expired,
  Inactive), then who redeemed it from the orders. Each name links to that
  attendee's detail sheet.
- Routes accept a query: `#/<code>/attendees?q=<search>&open=<attendee id>`
  pre-fills the search and opens the attendee's sheet.
- Remove the check-in button from the attendee sheet; the UI no longer writes
  to Cvent. The check-in status badges stay. The server's
  `POST /api/cvent/events/{code}/checkin` route is unchanged.

## v0.1.1 — 2026-09-26

- Remove `run.sh`. It skipped rebuilds once a binary existed and listened on
  all interfaces by default. `make serve ADDR=0.0.0.0:8766` replaces its LAN
  use; the default stays loopback-only.
- README: fix the broken Quick auth snippet, add a no-authentication
  warning, drop the Phone install section.
- `.env.example`: leave `CVENT_CLIENT_SECRET` empty (it held a literal `***`).
- `tests/test_server.sh`: default to ports 18766/18767 (overridable via
  `APP_PORT` / `DEAD_PORT`) and bind loopback, so it runs alongside
  `make serve`.

## v0.1.0 — 2026-09-26

First public release.

### Go package `cvent/` (stdlib only)
- OAuth2 client-credentials auth with token cache; credentials from the real
  environment or a repo-root `.env` (the environment wins per key).
- Cursor pagination and filter POSTs for the Cvent Platform REST API.
- Per-event fan-out fetch into a bundle (event, attendees, activities,
  orders, transactions, fee/admission items, registration types, discounts,
  sessions, event questions) with a 15-minute cache and in-flight dedup.
- Payment summary join (paid / partial / unpaid / refund) and a registrants
  export with per-attendee payment status.
- Snapshot dumps to `data/<code>/` with a merged `data/index.json` catalog.

### CLI `cmd/cvent-dump/`
- Dumps any number of events by code or uuid; defaults to every
  `CVENT_CODE_<n>` in `.env`.

### Server `server/` + PWA `web/`
- Event-scoped `/api/cvent/events/<code>/…` routes: bundle, payments,
  attendee search, check-in, repull + status; `/api/cvent/events` catalog.
- Starts without credentials: static serving and `/api/health` keep working,
  `/api/cvent/…` answers `503`.
- Vanilla-JS SPA: event picker, dashboard (registrations, pricing, payments,
  program), attendee search with detail sheet, light/dark themes.
- Content-hashed shell assets, service worker with offline banner, manifest
  and icons.

### Tests
- Offline server suite (`tests/test_server.sh`) against a local mock Cvent
  upstream with a synthetic fixture.
- Read-only live API smoke tests (`tests/test_*.sh`) and endpoint map
  (`tests/ENDPOINTS.md`).
