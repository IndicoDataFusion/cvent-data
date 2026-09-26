# Changelog

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
