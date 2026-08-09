#!/usr/bin/env bash
# Boot a hermetic fixture server for the e2e suite: fresh --data-dir, fixture
# .abooks imported, one PRISTINE work and one MESSY (merged multi-edition)
# work. Prints "READY <port> <pristine_work_id> <messy_work_id>" on success.
set -euo pipefail
PORT="${E2E_PORT:-8199}"
DIR="${E2E_DIR:-$(mktemp -d /tmp/abookify-e2e-XXXX)}"
mkdir -p "$DIR"
BIN="${E2E_BIN:-./bin/abookify-e2e}"
CLEAN_ABOOK="${E2E_CLEAN_ABOOK:?path to clean Carol .abook}"
HUMAN_ABOOK="${E2E_HUMAN_ABOOK:?path to LibriVox Carol .abook}"

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

P=$(curl -sf -X POST -F "file=@$CLEAN_ABOOK" "http://localhost:$PORT/api/import" | python3 -c "import json,sys; print(json.load(sys.stdin)['work_id'])")
H=$(curl -s -X POST -F "file=@$HUMAN_ABOOK" "http://localhost:$PORT/api/import" | python3 -c "
import json,sys
try: print(json.load(sys.stdin).get('work_id',''))
except Exception: print('')")
# NOTE: importing a same-title .abook 409s on identity dedupe — the messy
# same-book-two-narrations fixture needs an import force flag (tracked in the
# handoff); until then the second fixture should be a DIFFERENT title.
# The messy fixture: merge the human work INTO a copy-shape alongside the TTS
# one — multiple narrations + multiple texts in one work, PJ's real shape.
curl -sf -X POST -H "Content-Type: application/json" -d "{\"source_id\":$H}" \
  "http://localhost:$PORT/api/works/$P/merge" >/dev/null && MESSY=$P || MESSY=""
echo "READY $PORT ${MESSY:+$MESSY} pristine=$P human=$H dir=$DIR"
