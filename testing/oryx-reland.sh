#!/bin/bash
# ONE COMMAND for the day PJ re-acquires Oryx and Crake's four truncated
# files (01/04/09/10.mp3). Staged 2026-08-10 while the repair sat blocked,
# so his part is genuinely one step: replace the files, run this.
#
#   ./testing/oryx-reland.sh
#
# The repair itself already reached 2067->0 fabrications on 2026-08-06;
# only the LANDING was refused, because transcribing a truncated file
# silently loses everything after the damage. This script refuses the
# same way until the files are actually fixed (step 0 is a fast local
# check; stt-cli's built-in damage preflight remains the authority).
set -u
cd "$(dirname "$0")/.."
AUDIO="testdata/library/audiobooks/Margaret Atwood - Oryx and Crake"
DB=./data/abookify.db
LIB=./testdata/library
WORK=57
CLI=../server-transcription/bin/stt-cli   # the binary the 08-06 repair ran
[ -x "$CLI" ] || CLI=./bin/stt-cli

echo "== step 0: truncation pre-check (zero-run ending on a MiB boundary) =="
bad=0
for f in "$AUDIO"/*.mp3; do
  python3 - "$f" <<'PY' || bad=1
import sys
p=sys.argv[1]
data=open(p,'rb').read()
tail=data[-1024*1024:]
z=len(tail)-len(tail.rstrip(b'\x00'))
if z>512*1024:
    print(f"  STILL TRUNCATED: {p.split('/')[-1]} ({z} trailing zero bytes)"); sys.exit(1)
print(f"  ok: {p.split('/')[-1]}")
PY
done
if [ "$bad" = 1 ]; then
  echo "REFUSING: re-acquire the files above first (interrupted downloads)."
  echo "Nothing was changed."
  exit 2
fi

echo "== step 1: whisper up? =="
curl -sf -m 5 http://localhost:5200/health >/dev/null || { echo "whisper not up — run: make whisper"; exit 3; }

echo "== step 2: transcribe (stt-cli's damage preflight is the authority) =="
# Full re-decode: bootstrap a fresh sidecar, then per-file passes — the same
# invocation shape repair-library.sh used, all 11 files (the 7 good ones
# re-decode identically; the 4 new ones decode for the first time).
rm -f "${AUDIO}.stt.json"
$CLI -audio "$AUDIO" -bootstrap-sidecar || exit 4
for f in "$AUDIO"/*.mp3; do
  fn=$(basename "$f")
  echo "  -- $fn"
  $CLI -audio "$AUDIO" -redo-files "$fn" || exit 4
done

echo "== step 3: land + verify (rewrites alignment, stamps content_version) =="
./bin/reimport-realign -db "$DB" -library "$LIB" -work $WORK || exit 5

echo "== step 4: coherence for work $WORK =="
TOK=$(python3 -c "
import sqlite3
con=sqlite3.connect('file:./data/abookify.db?mode=ro',uri=True)
print(con.execute('select token from auth_sessions order by created_at desc limit 1').fetchone()[0])")
curl -s -m 120 -H "Authorization: Bearer $TOK" http://localhost:7654/api/coherence | python3 -c "
import json,sys
for w in json.load(sys.stdin).get('works',[]):
    if w.get('work_id')==$WORK:
        ok=w.get('coherent',True)
        print('work $WORK coherent:',ok)
        sys.exit(0 if ok else 6)"
echo "== DONE — Oryx and Crake landed =="
