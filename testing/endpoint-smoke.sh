#!/bin/bash
# Endpoint status-contract smoke suite. Hits each major route against a running
# server and asserts a sensible status — 2xx, or a graceful 4xx/503 with a JSON
# error — but NEVER a bare 500. Locks the cast graceful-degradation contract
# (extract-cast returns 503, not 500, when the opt-in BookNLP service is down).
#
# Usage: ./testing/endpoint-smoke.sh [base-url]
# Env:   ABOOKIFY_TOKEN  optional bearer token (server with auth enabled)
set -u
BASE="${1:-http://localhost:7654}"
SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"

if ! curl -sSf -m 5 "$BASE/api/health" > /dev/null; then
  echo "✗ server unreachable at $BASE — start it (docker compose up -d) first"
  exit 1
fi

exec python3 "$SCRIPT_DIR/endpoint_smoke.py" "$BASE"
