# cvent-data

Client, tests, and reference material for the Cvent Platform REST API, plus
a PWA for browsing event data. Go module `github.com/IndicoDataFusion/cvent-data`.

- `cvent/` — Go package (stdlib only): credentials, API client, event
  bundle cache, payments join, snapshot dumps.
- `cmd/cvent-dump/` — standalone CLI that writes event snapshots.
- `server/` + `web/` — the Cvent Data PWA (see below).
- `openapi.json` (gitignored; fetch with `make openapi`) — the official
  OpenAPI 3.0.2 spec (367 endpoints) from the Cvent developer portal. Use it
  as the source of truth for endpoints, schemas, and scopes.
- `tests/` — bash smoke tests against the live API (see `tests/README.md`).
- `scripts/make_icons.py` — regenerates the PWA icons in `web/icons/`.
- `.env` (gitignored; template in `.env.example`) — `CVENT_CLIENT_ID` /
  `CVENT_CLIENT_SECRET` and the event list `CVENT_CODE_1`, `CVENT_CODE_2`, ….

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

---

## Cvent Data PWA

A PWA built on top of the API above: a Go server (stdlib only)
serves a vanilla-JS SPA plus a small `/api/cvent/…` surface. The event
picker lists the events in the dump catalog (`data/index.json`); the
`--event` code, else `CVENT_CODE_1`, is the default selection.

### Run

```bash
make serve    # build ./data-server, dump data/ if missing, serve on 127.0.0.1:8766
make dump     # re-pull every configured event's snapshot into data/
```

Serves the app at **http://localhost:8766**. `make serve` rebuilds
`./data-server` (`-X main.buildSHA=<short sha>`) and, when
`data/index.json` is absent, runs a one-time dump first — the event list
(`GET /api/cvent/events`) is read from that file, so without it the UI shows
no events. `bash run.sh` is the older LAN variant: it builds
`cvent-data-server` if missing and listens on `${ADDR:-:8766}` (all
interfaces), but does not dump.

Events are listed as **`CVENT_CODE_1`**, **`CVENT_CODE_2`**, … in the
repo-root `.env` (the real environment wins per key). Dumps pull all of
them; the first is the default selection (the `--event` flag always wins).
With none set, a legacy `CVENT_EVENT` is used; with nothing configured a
dump exits with an error and the picker defaults to the first catalog
event. To add an event, add the next `CVENT_CODE_<n>`
and run `make dump`; the picker lists it on the next page load (restart the
server only to change the default). The picker shows each event's optional
`shortName` from `data/index.json`, else its full title — set it by hand in
that file; re-dumps preserve it. Credentials — `CVENT_CLIENT_ID` /
`CVENT_CLIENT_SECRET` — live in the gitignored `.env`. `CVENT_API_BASE` is
optional (default `https://api-platform.cvent.com/ea`; the EU base is
`https://api-platform-eur.cvent.com/ea`).

With **missing credentials** the server still starts: static serving and
`/api/health` keep working, but `/api/cvent/…` answers
`503 {"error":"cvent credentials not configured"}` (the `cvent` source is
always registered; without a client its handler group reports missing
credentials).

### Phone install

The app is a PWA (manifest + service worker; add-to-home-screen works), but
this host serves plain HTTP on the LAN. Browsers require HTTPS (or
localhost) to register a service worker, so on a LAN phone the SW will not
register and offline mode is unavailable — the app still works fully online
from the phone. Serve it behind HTTPS (or use
static mode below) for the full PWA experience.

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
…) and
`meta.json` — then exits (no server). It is one-shot and fatal on missing
credentials. The server serves that dir at `/data/…`; the frontend reads it
instead of the live API when the page URL has a `static=1` query param or
the host is `*.github.io` (a **Snapshot** badge appears and Re-pull /
check-in are hidden). Static mode is
the intended path for HTTPS-deployed or file-only setups.

### Web app

An event picker at the left of the top bar, then per-event tabs:
**Overview** (a dashboard of collapsible Registrations, Pricing, Payments
and Program sections) and **Attendees** (search, a table, and a detail
sheet with the full record — registration answers labelled with their
question text (from `GET /event-questions`) — plus a check-in button that writes back to
Cvent; hidden in static mode). The footer shows the build SHA.

### API

| Route | Notes |
|---|---|
| `GET /api/health` | `{ok, source, build}`; always 200 |
| `GET /api/cvent/events` | `{events, default}`: the catalog from `data/index.json` (empty if no dump) + the default code (`--event`, else `CVENT_CODE_1`) |
| `GET /api/cvent/events/{code}` | the 13-resource bundle; 15-min server cache |
| `GET /api/cvent/events/{code}/payments` | attendee→order→transaction join: `totals` (ordered/paid/due/refunded) + `orders` rows + `cancelled` |
| `GET /api/cvent/events/{code}/attendees?q=&limit=&offset=` | case-insensitive search over name/email/confirmation; `limit` defaults 50, caps at 200. `questions` maps each answered question id → `{text, type}` |
| `POST /api/cvent/events/{code}/checkin` | body `{"attendeeIds":[…]}`; max 100; **writes to Cvent**. The app has no auth of its own — LAN trust model |
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
