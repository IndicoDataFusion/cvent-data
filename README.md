# cvent-data

Client, tests, and reference material for the Cvent Platform REST API, plus
a PWA for browsing event data. Go module `github.com/IndicoDataFusion/cvent-data`.

- `cvent/` — Go package (stdlib only): credentials, API client, event
  bundle cache, payments join, snapshot dumps.
- `cmd/cvent-dump/` — standalone CLI that writes event snapshots.
- `cmd/cvent-sheets-export/` — CLI that exports registrants to CSV and Google
  Sheets (`internal/gsheets`); each release attaches its binaries (see
  below).
- `server/` + `web/` — the Cvent Data PWA (see below).
- `openapi.json` (gitignored; fetch with `make openapi`) — the official
  OpenAPI 3.0.2 spec (367 endpoints) from the Cvent developer portal. Use it
  as the source of truth for endpoints, schemas, and scopes.
- `tests/` — bash smoke tests against the live API (see `tests/README.md`).
- `CHANGELOG.md` — release notes.
- `scripts/make_icons.py` — regenerates the PWA icons in `web/icons/`.
- `.env` (gitignored; template in `.env.example`) — `CVENT_CLIENT_ID` /
  `CVENT_CLIENT_SECRET` and the event list `CVENT_CODE_1`, `CVENT_CODE_2`, ….

## Quick auth

```bash
set -a; . ./.env; set +a
TOKEN=$(curl -s -X POST "https://api-platform.cvent.com/ea/oauth2/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -H "Authorization: Basic $(printf '%s' "$CVENT_CLIENT_ID:$CVENT_CLIENT_SECRET" | base64 | tr -d '\n')" \
  -d "grant_type=client_credentials&client_id=$CVENT_CLIENT_ID" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')

curl -s "https://api-platform.cvent.com/ea/events?limit=5" \
  -H "Authorization: Bearer $TOKEN"
```

## Use as a Go library

```bash
go get github.com/IndicoDataFusion/cvent-data@latest
```

The `cvent` package has no dependencies beyond the standard library. Its
attendee record is `cvent.Registrant` — the same struct the web app's
attendee search returns and the dump writes to `registrants.json`. To read
an event's attendees for matching against another system (this is how
[IndicoDataFusion](https://github.com/IndicoDataFusion/IDF) pairs Cvent
attendees with Indico registrations):

```go
import "github.com/IndicoDataFusion/cvent-data/cvent"

c := cvent.New(cvent.DefaultBaseURL, clientID, clientSecret)
atts, err := cvent.FetchRegistrants(ctx, c, "EXAMPLECODE", cvent.RegistrantsOptions{
	WithPayments:       true, // also read orders, order items + transactions
	WithBillingAddress: true, // also read each charge's billing address
})
for _, a := range atts {
	// a.FullName, a.Email, a.Status, a.Ref (the Indico Ref # answer, trimmed),
	// a.RegistrationType (the member type), a.Company, a.Title, a.MobilePhone,
	// a.EmergencyContactName / Phone, a.VisaLetter ("Yes" | "No"),
	// a.Address (+ AddressType, OtherAddress), a.InvoiceAddress() (billing
	// address, else a.Address), the other contact fields (Prefix, CCEmail,
	// WorkPhone, …), AdmissionItem, RegisteredAt, CheckedInAt,
	// a.Guests / a.RegisteredBy (guest links; a guest has Guest + PrimaryID),
	// a.Answers (question text + value), a.CustomFields (every contact
	// custom field).
	// a.Payment (nil without an order): Status is paid | partial | unpaid |
	// waived | cancelled, plus Ordered/Paid/Due/Refunded, Currency, Method,
	// LastPaymentDate, Invoices, Discounts, OriginalAmount (before discounts)
	// and Items (the selected order lines; a guest's fee has ForGuest).
}
```

- `FetchRegistrants` resolves a short event code or uuid, then fetches only
  attendees and event questions (plus orders, order items and transactions
  with `WithPayments`), not the full bundle.
- Three other ways to the same `[]Registrant`, all built by
  `BuildRegistrants`: `RegistrantsFromBundle(bundle, "")` over a cached
  `EventCache` bundle (payments and billing addresses included); a dump's
  `<code>/registrants.json`; or the app's
  `GET /api/cvent/events/{code}/attendees`, whose `items` decode straight
  into `cvent.Registrant`.
- The emergency contact and visa-letter answers are contact custom fields,
  not event questions; they are matched by field name
  (`EmergencyContactNamePattern`, `EmergencyContactPhonePattern`,
  `VisaLetterPattern`).
- Guests are attendees of their own (`Guest`, `PrimaryID`) and are listed on
  their primary in `Guests`. A guest's fee is a line on the primary's order,
  so guests have no `Payment`.
- Billing addresses exist only on the transaction detail, so
  `WithBillingAddress` makes one `GET /transactions/{id}` per successful
  charge. Card payments carry one; bank transfers do not, and
  `InvoiceAddress()` then falls back to the contact address.
- `Ref` is the answer to the question whose text matches
  `DefaultRefQuestionPattern` ("Indico … Ref"/"reference"); pin another
  question with `RegistrantsOptions.RefQuestionID`.
- Payment status rules: every order cancelled → `cancelled`; nothing
  charged (e.g. a 100% discount) → `waived`; nothing due → `paid`; some
  paid → `partial`; otherwise `unpaid`. Orders carry no currency, so it
  comes from the transactions, else the event.
- `FetchDiscounts(ctx, c, code)` returns the event's discount codes as
  `[]Discount` with one paged `GET /events/{id}/discounts`: code, name,
  `Method` (`BY_PERCENTAGE`, `BY_AMOUNT`, `FLAT_PRICE`) and `Value`, active
  flag, validity dates, and the redemption count `CapacityUsed` of
  `CapacityTotal` (-1 = unlimited; see `Unlimited()` and `Remaining()`).
  Orders name the codes they used in `AttendeePayment.Discounts[].Code`.
- Pure helpers for data you already hold: `BuildRegistrants`,
  `RegistrantsFrom`, `LabelAnswers`, `RefQuestionID`, `PaymentsByAttendee`,
  `OrderItemsByAttendee`, `DiscountsFrom`, and `BuildPayments` (the event-wide
  money view). `FromEnvironment` reads credentials from the environment or
  a `.env`; `NewEventCache(c).Bundle(ctx, code)` returns the full 13-resource
  bundle.

---

## Export registrants to CSV and Google Sheets

`cvent-sheets-export` fetches events' registrants live from Cvent (payments
and billing addresses included). It writes one `<dir>/<code>.csv` per event
and, optionally, replaces a Google Sheet tab with the same rows. In the
sheet, the header row is frozen and values are written as plain text. The
columns are those of `cvent.RegistrantsTable`.

```bash
go run ./cmd/cvent-sheets-export --dir exports                 # every CVENT_CODE_<n>
go run ./cmd/cvent-sheets-export --match '<title word>'        # events whose title matches
go run ./cmd/cvent-sheets-export <CODE> --dir "" \
  --sheet-id <spreadsheet id> --sheet-tab Registrants          # sheet only, no CSV
```

- `--sheet-id-env <VAR>` reads the spreadsheet id from the environment or
  `.env`.
- Without `--sheet-tab`, each event goes to a tab named after its code.
- `--info-tab info` also writes a two-column `key`/`value` summary: to
  that tab, and to `<dir>/info.csv` when CSV files are written. It holds
  `exported_at` (UTC), `exporter` (version), `tables`, `rows_total`, then
  per event tab `<tab>.rows`, `<tab>.previous_rows`, `<tab>.rows_delta` and
  the registrants `<tab>.added`, `<tab>.removed` and `<tab>.changed` (with
  the changed columns) since the previous version. The previous version is
  the tab being replaced, else the CSV file being overwritten. Rows match by
  `id`; each list names at most 100 registrants.
- `--info KEY=VALUE` (repeatable) adds your own rows after `rows_total`,
  e.g. `--info source=nightly-ci`. It implies `--info-tab info`.
- `--highlight` marks the changes in each event tab, compared with the
  tab's previous contents: added rows light green, changed cells light
  yellow with the previous value as a cell note. Each run first clears the
  tab's background colours and notes, so only the latest changes show. A
  first export (no previous tab) is not highlighted.
- `-h` lists every flag.

**Google setup (once):**

1. In a Google Cloud project, enable the **Google Sheets API**.
2. Create a **service account** and add a **JSON key**. Point
   `GOOGLE_SHEETS_KEY_FILE` at it, kept outside the repo and
   `chmod 600`. In CI, put the key's JSON in `GOOGLE_SHEETS_KEY_JSON`.
3. **Share** the spreadsheet with the key's `client_email` as **Editor**.

The output holds attendee personal and payment data. Share sheets with
named people only, never "anyone with the link".

**Release binaries:** pushing a `v*` tag runs
`.github/workflows/release.yml`. It tests, runs `make dist` (static
`cvent-sheets-export_<os>_<arch>` binaries for Linux and macOS, plus
`SHA256SUMS`) and attaches them to that tag's GitHub release. Pin a release
and verify the checksum to run the exporter elsewhere, for example from a
scheduled workflow, without building it.

## Cvent Data PWA

A PWA built on top of the API above: a Go server (stdlib only)
serves a vanilla-JS SPA plus a small `/api/cvent/…` surface. The event
picker lists the events in the dump catalog (`data/index.json`); the
`--event` code, else `CVENT_CODE_1`, is the default selection.

### Run

```bash
make serve    # build ./data-server, dump data/ if missing, serve on 127.0.0.1:8766
make dump     # re-pull every configured event's snapshot into data/
make serve ADDR=0.0.0.0:8766   # also reachable from other devices on the LAN
```

Open **http://localhost:8766**. `make serve` rebuilds `./data-server` and,
when `data/index.json` is absent, runs a one-time dump first — the event
list is read from that file.

> **The app has no authentication.** Anyone who can reach the port can read
> attendee and payment data and check attendees in (a write to Cvent). Keep
> the default loopback address unless you trust the network.

Events are listed as **`CVENT_CODE_1`**, **`CVENT_CODE_2`**, … in `.env`
(the real environment wins per key). Dumps pull all of them; the first is
the default selection (`--event` overrides it). To add an event, add the
next `CVENT_CODE_<n>` and run `make dump`. The picker shows each event's
optional `shortName` from `data/index.json`, else its title — set it by hand
in that file; re-dumps preserve it. `CVENT_API_BASE` is optional (default
`https://api-platform.cvent.com/ea`; EU: `https://api-platform-eur.cvent.com/ea`).

Without credentials the server still starts: static files and `/api/health`
work, and `/api/cvent/…` answers `503`.

### Offline / static mode

```bash
./data-server --dump data                        # every CVENT_CODE_<n>; or: make dump
./data-server --event <CODE> --dump data        # just one event
go run ./cmd/cvent-dump --dir data               # standalone CLI, same layout
go run ./cmd/cvent-dump <CODE1> <CODE2>          # explicit codes
```

`--dump <dir>` fetches each configured event **once** and writes a snapshot —
`<dir>/index.json` plus one dir per event holding `event.json`, 13 resource
files (attendees, orders, transactions, sessions, speakers, event questions,
…), `registrants.json` (the `cvent.Registrant` records the Attendees view
reads), `registrants.csv` (the same records as a spreadsheet) and
`meta.json` — then exits (no server). It is one-shot and fatal on missing
credentials. The server serves that dir at `/data/…`; the frontend reads it
instead of the live API when the page URL has a `static=1` query param or
the host is `*.github.io` (a **Snapshot** badge appears and Re-pull is
hidden). Static mode is
the intended path for HTTPS-deployed or file-only setups.

### Web app

An event picker at the left of the top bar, then per-event tabs:
**Overview** (a dashboard of collapsible Registrations, Pricing, Discounts,
Payments and Program sections; Discounts shows each code's used / total and
links each redemption to its attendee) and **Attendees** (search, a table, and a detail
sheet with the full `cvent.Registrant` — every contact field, the
registration, guest ↔ primary links, the order lines with payment status and
billing address, answers labelled with their question text, and a read-only
check-in status). The footer shows the build SHA.

### API

| Route | Notes |
|---|---|
| `GET /api/health` | `{ok, source, build}`; always 200 |
| `GET /api/cvent/events` | `{events, default}`: the catalog from `data/index.json` (empty if no dump) + the default code (`--event`, else `CVENT_CODE_1`) |
| `GET /api/cvent/events/{code}` | the 13-resource bundle plus `billingAddresses` (attendee id → address, from the transaction details); 15-min server cache |
| `GET /api/cvent/events/{code}/payments` | attendee→order→transaction join: `totals` (ordered/paid/due/refunded) + `orders` rows + `cancelled` |
| `GET /api/cvent/events/{code}/attendees?q=&limit=&offset=` | case-insensitive search over name/email/confirmation; `limit` defaults 50, caps at 200. `items` are `cvent.Registrant` records |
| `GET /api/cvent/events/{code}/attendees.csv?q=` | every match (no paging) as a CSV download (`cvent.WriteRegistrantsCSV`): one row per registrant, payment summary, plus a `Q: …` column per event question and a `Field: …` column per contact custom field. The Attendees view links it as **Export CSV** |
| `POST /api/cvent/events/{code}/repull` | background re-fetch; returns immediately (non-blocking) |
| `GET /api/cvent/events/{code}/repull-status` | `{running, pulledAt}` |

Errors are JSON. **502** = upstream Cvent failure (event resolution or a
resource fetch); **503** = no cvent creds (see above); **404** = unknown
source.

### Tests

- `tests/run_all.sh` — every `tests/test_*.sh`. Most hit the **live** Cvent
  API; `tests/test_server.sh` is self-contained and offline — it runs the
  real binary against a local mock upstream (no network), with its synthetic
  fixture generated by `tests/make_fixture.py`.
- Go unit tests: `go test ./... -race` (from the repo root; `go.mod` lives there).

### Adding a data source

The app is multi-source by contract. **Go side:** add a handler group under
`/api/<source>/…` and register it in the `sources` map in `server/main.go`
(the `cvent` group in `server/handlers.go` is the reference). **Web side:**
add `web/sources/<name>.js` exporting the source's views + data access, and
one line in `web/sources/registry.js`:

```js
mydata: { label: "My Data", views: { home: mydata.home }, staticDataPath: "mydata/" },
```

`web/app.js` builds its routing from the registry, so that is the only
other file touched. You inherit for free: the shell (top bar, themes,
offline banner), the `sw.js` caching patterns (just extend the route list),
dump/static mode (point `staticDataPath` at the source's dump layout), and
the icons/manifest.

## License

MIT — see `LICENSE`. The bundled Manrope and Space Grotesk fonts in
`web/fonts/` are under the SIL Open Font License 1.1 (`web/fonts/OFL-*.txt`).
