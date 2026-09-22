#!/usr/bin/env bash
# Build (if needed) and run the Cvent Data PWA server
cd "$(dirname "$0")"
[ -x cvent-data-server ] || (cd server && go build -ldflags "-X main.buildSHA=$(git rev-parse --short=7 HEAD 2>/dev/null)" -o ../cvent-data-server .)
exec ./cvent-data-server --addr "${ADDR:-:8766}" --web web --data data
