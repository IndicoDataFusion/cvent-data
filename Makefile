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

# Release binaries of the exporter (cmd/cvent-sheets-export), static, one
# per platform, plus SHA256SUMS, in dist/. VERSION is the tag being
# released; .github/workflows/release.yml attaches them to the release.
VERSION ?= $(shell git describe --tags --always --dirty)
DIST_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
dist:
	rm -rf dist && mkdir -p dist
	for p in $(DIST_PLATFORMS); do \
	  CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} go build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION)" \
	    -o dist/cvent-sheets-export_$${p%/*}_$${p#*/} ./cmd/cvent-sheets-export || exit 1; \
	done
	cd dist && sha256sum cvent-sheets-export_* > SHA256SUMS

.PHONY: build dump serve openapi dist

# Fetch the official Cvent OpenAPI spec (unauthenticated; gitignored).
openapi:
	curl -sf -X POST https://developers.cvent.com/api/graphql \
	  -H 'Content-Type: application/json' -d '{"query":"{ getPublicSpec }"}' \
	  | python3 -c 'import json,sys;s=json.load(sys.stdin)["data"]["getPublicSpec"];s=json.loads(s) if isinstance(s,str) else s;json.dump(s,open("openapi.json","w"),indent=2)'
