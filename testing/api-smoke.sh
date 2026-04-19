#!/bin/bash
# API-level smoke test. Exercises the three bug classes the user caught:
#   1. Chapter list endpoint must return a JSON array (not {error})
#   2. Sync data must have non-zero word entries (schema match)
#   3. Chapter content must have text/content_html when word_count > 0
#
# Runs against a running server. No browser needed.
#
# Usage: ./testing/api-smoke.sh [base-url]

set -u
BASE="${1:-http://localhost:7654}"

SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"
if ! curl -sSf -m 5 "$BASE/api/health" > /dev/null; then
  echo "✗ health endpoint unreachable at $BASE"
  exit 1
fi
echo "✓ health endpoint"

exec python3 "$SCRIPT_DIR/api_smoke.py" "$BASE"
