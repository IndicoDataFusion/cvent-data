# Common helpers for Cvent API test scripts.
# Source this from test scripts: source "$(dirname "$0")/lib.sh"

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$REPO_ROOT/.env"
API_BASE="${CVENT_API_BASE:-https://api-platform.cvent.com/ea}"
TOKEN_CACHE="${TOKEN_CACHE:-/tmp/cvent_token.$(id -u).json}"

log()  { printf '\033[1;34m[tests]\033[0m %s\n' "$*"; }
pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; }

die() { fail "$*"; exit 1; }

require_env() {
  [ -f "$ENV_FILE" ] || die ".env not found at $ENV_FILE (expected CVENT_CLIENT_ID / CVENT_CLIENT_SECRET)"
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
  [ -n "${CVENT_CLIENT_ID:-}" ] && [ -n "${CVENT_CLIENT_SECRET:-}" ] \
    || die "CVENT_CLIENT_ID / CVENT_CLIENT_SECRET not set in $ENV_FILE"
}

# Print a valid access token, cached while still fresh.
cvent_token() {
  if [ -f "$TOKEN_CACHE" ]; then
    local exp now
    exp=$(python3 -c 'import json;print(json.load(open("'"$TOKEN_CACHE"'"))["expires_at"])' 2>/dev/null || echo 0)
    now=$(date +%s)
    if [ "$exp" -gt "$now" ]; then
      python3 -c 'import json;print(json.load(open("'"$TOKEN_CACHE"'"))["access_token"])'
      return 0
    fi
  fi
  local resp
  resp=$(curl -sf --max-time 30 -X POST "$API_BASE/oauth2/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    -H "Authorization: Basic $(printf '%s' "$CVENT_CLIENT_ID:$CVENT_CLIENT_SECRET" | base64 | tr -d '\n')" \
    --data "grant_type=client_credentials&client_id=$CVENT_CLIENT_ID") \
    || die "token endpoint returned an error (check credentials/region)"
  local tmp; tmp=$(mktemp)
  printf '%s' "$resp" > "$tmp"
  python3 -c '
import json, time, sys
d = json.load(open(sys.argv[1]))
d["expires_at"] = int(time.time()) + int(d.get("expires_in", 3600)) - 30
json.dump(d, open(sys.argv[1], "w"))
print(d["access_token"])
' "$tmp"
  rm -f "$tmp"
}

# cvent_get <path> [extra query...] -> JSON body on stdout
cvent_get() {
  local path="$1"; shift || true
  local url="$API_BASE$path"
  if [ "$#" -gt 0 ]; then url="$url?$*"; fi
  curl -sf --max-time 60 "$url" \
    -H "Authorization: Bearer $(cvent_token)" \
    -H 'Accept: application/json'
}

# Check a condition: check <condition-exit-code> <label>
check() {
  if [ "$1" -eq 0 ]; then pass "$2"; else fail "$2"; exit 1; fi
}
