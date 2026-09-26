SHA1 := $(shell git rev-parse --short=7 HEAD)
OUT := $(shell realpath ./data-server)
DATA := data
# Listen address. Loopback by default: the app has no auth. For a phone on
# the LAN, opt in with: make serve ADDR=0.0.0.0:8766
ADDR ?= 127.0.0.1:8766

build:
	cd server && go build -ldflags "-X main.buildSHA=$(SHA1)" -o $(OUT)

# The event catalog (/api/cvent/events) is read from $(DATA)/index.json, which
# only a dump writes; without it the UI has no events. Dump once if missing.
$(DATA)/index.json:
	$(MAKE) dump

# Re-pull the snapshots (every CVENT_CODE_<n> in .env).
dump: build
	$(OUT) --dump $(DATA)

serve: build $(DATA)/index.json
	$(OUT) --addr $(ADDR) --web web --data $(DATA)

.PHONY: build dump serve openapi

# Fetch the official Cvent OpenAPI spec (unauthenticated; gitignored).
openapi:
	curl -sf -X POST https://developers.cvent.com/api/graphql \
	  -H 'Content-Type: application/json' -d '{"query":"{ getPublicSpec }"}' \
	  | python3 -c 'import json,sys;s=json.load(sys.stdin)["data"]["getPublicSpec"];s=json.loads(s) if isinstance(s,str) else s;json.dump(s,open("openapi.json","w"),indent=2)'
