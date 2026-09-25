SHA1 := $(shell git rev-parse --short=7 HEAD)
OUT := $(shell realpath ./data-server)
DATA := data

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
	$(OUT) --addr 127.0.0.1:8766 --web web --data $(DATA)

.PHONY: build dump serve
