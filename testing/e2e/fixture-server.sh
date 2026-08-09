#!/usr/bin/env bash
# Hermetic e2e fixture server. IMPORTS FAIL LOUDLY (mobile lost a calibration
# run to a silently-swallowed import error — never again): any fixture that
# doesn't yield a work id aborts the boot with the server's response printed.
#
#   E2E_CLEAN_ABOOK=... E2E_SECOND_ABOOK=... [E2E_MESSY_ABOOK=...] \
#     testing/e2e/fixture-server.sh
#
# Prints:  READY <port> clean=<id> second=<id> [messy=<id>] dir=<dir>
# The default fixture is UNMERGED: work 1 = clean single-narration Carol
# (word-synced by construction), work 2 = a DIFFERENT title (same-title
# imports 409 on identity dedupe). The known-broken multi-edition state is
# the separate E2E_MESSY_ABOOK (full export of PJ's work 85, mangled
# transcript preserved deliberately). Merging is NOT done here — the merge
# experiment surfaced merge-kills-playback and lives in the calibration
# notes, not the default fixture.
set -euo pipefail
PORT="${E2E_PORT:-8199}"
DIR="${E2E_DIR:-$(mktemp -d /tmp/abookify-e2e-XXXX)}"
mkdir -p "$DIR"
BIN="${E2E_BIN:-./bin/abookify-e2e}"
CLEAN_ABOOK="${E2E_CLEAN_ABOOK:?path to clean single-narration .abook}"
SECOND_ABOOK="${E2E_SECOND_ABOOK:?path to a different-title .abook}"
MESSY_ABOOK="${E2E_MESSY_ABOOK:-}"

cd "$(dirname "$0")/../.."
if [ ! -x "$BIN" ]; then
  docker run --rm -v "$(pwd)":/app -w /app -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
    golang:1.24-bookworm go build -o "$BIN" ./cmd/abookify
fi
setsid nohup "$BIN" --data-dir "$DIR" --port "$PORT" > "$DIR/server.log" 2>&1 < /dev/null &
for i in $(seq 1 60); do
  curl -sf -m 2 "http://localhost:$PORT/api/ready" >/dev/null 2>&1 && break
  sleep 2
done
curl -sf -m 2 "http://localhost:$PORT/api/ready" >/dev/null

import_or_die() { # path -> work id on stdout, aborts loudly on anything else
  local body
  body=$(curl -s -X POST -F "file=@$1" "http://localhost:$PORT/api/import")
  local id
  id=$(printf '%s' "$body" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin); print(d.get('work_id',''))
except Exception:
    print('')" )
  if [ -z "$id" ]; then
    echo "FIXTURE IMPORT FAILED for $1" >&2
    echo "server said: $body" >&2
    exit 1
  fi
  printf '%s' "$id"
}

CLEAN_ID=$(import_or_die "$CLEAN_ABOOK")
SECOND_ID=$(import_or_die "$SECOND_ABOOK")
MESSY_ID=""
if [ -n "$MESSY_ABOOK" ]; then MESSY_ID=$(import_or_die "$MESSY_ABOOK"); fi
echo "READY $PORT clean=$CLEAN_ID second=$SECOND_ID ${MESSY_ID:+messy=$MESSY_ID }dir=$DIR"
