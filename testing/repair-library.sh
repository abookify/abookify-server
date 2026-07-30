#!/usr/bin/env bash
# Library repair: re-transcribe books carrying fabricated text, worst first.
#
# Worst-first so the biggest correctness win lands earliest and the run can be
# stopped at any point with the remainder being the least affected books.
#
# RESUME GRANULARITY IS PER FILE, NOT PER BOOK. The first version recorded a book
# as done only once every file finished, which meant a crash 7 hours into Atlas
# Shrugged (53 h of audio) discarded all 7 hours. Multi-file books are therefore
# bootstrapped first — stt-cli --bootstrap-sidecar writes the sources and
# durations with no words — and then filled one file at a time with
# --redo-files. Each completed file is recorded, so an interruption costs the
# file in flight (~10-30 min) rather than the book (up to 8 h).
#
# Bootstrapping all sources up front also keeps --redo-files' offset guard happy:
# the sidecar records every file in the directory, so the recomputed timeline
# matches and the run is not refused.
set -uo pipefail
cd /home/pj/projects/jarvis/abookify/engineering/server
S=/home/pj/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad
ORDER="$S/repair_order.tsv"; DONE="$S/repair-done.txt"; FILEDONE="$S/repair-files-done.txt"
PROG="$S/repair-progress.tsv"; BK="$S/repair-backups"; LOGS="$S/repair-logs"
CLI=../server-transcription/bin/stt-cli
mkdir -p "$BK" "$LOGS"; touch "$DONE" "$FILEDONE" "$PROG"

fabricated() {
  python3 -c "
import json,sys
from collections import Counter
try: d=json.load(open(sys.argv[1]))
except Exception: print(-1); sys.exit()
c=Counter(x['s'] for x in d.get('words',[]))
print(sum(v for v in c.values() if v>6))
" "$1" 2>/dev/null || echo -1
}

while IFS=$'\t' read -r aff dur sidecar audiodir; do
  name=$(basename "$sidecar" .stt.json)
  grep -Fxq "$name" "$DONE" && continue

  audio=""
  if [ -n "$audiodir" ] && [ -d "$audiodir" ]; then
    audio="$audiodir"
  else
    base="${sidecar%.stt.json}"
    for ext in mp3 m4a m4b flac wav ogg opus; do
      [ -f "$base.$ext" ] && { audio="$base.$ext"; break; }
    done
  fi
  if [ -z "$audio" ]; then
    printf '%s\tORPHAN\t%s\t-\t-\n' "$(date +%H:%M:%S)" "$name" >> "$PROG"
    echo "$name" >> "$DONE"; continue
  fi

  before=$(fabricated "$sidecar")
  [ -f "$BK/$name.stt.json" ] || cp "$sidecar" "$BK/$name.stt.json" 2>/dev/null
  start=$(date +%s)

  if [ -d "$audio" ] && [ "$(find "$audio" -maxdepth 1 -type f \( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \) | wc -l)" -ge 2 ]; then
    # Multi-file: bootstrap once, then fill file by file so progress survives.
    if ! grep -Fq "$name	__bootstrapped__" "$FILEDONE"; then
      rm -f "$sidecar"
      if ! "$CLI" -audio "$audio" -bootstrap-sidecar >> "$LOGS/$name.log" 2>&1; then
        printf '%s\tBOOTSTRAP_FAIL\t%s\t-\t-\n' "$(date +%H:%M:%S)" "$name" >> "$PROG"
        echo "$name" >> "$DONE"; continue
      fi
      printf '%s\t__bootstrapped__\n' "$name" >> "$FILEDONE"
    fi
    rc=0
    while IFS= read -r f; do
      fn=$(basename "$f")
      grep -Fq "$name	$fn" "$FILEDONE" && continue
      if "$CLI" -audio "$audio" -redo-files "$fn" >> "$LOGS/$name.log" 2>&1; then
        printf '%s\t%s\n' "$name" "$fn" >> "$FILEDONE"
      else
        rc=1
        printf '%s\tFILE_FAIL\t%s\t%s\t-\n' "$(date +%H:%M:%S)" "$name" "$fn" >> "$PROG"
      fi
    done < <(find "$audio" -maxdepth 1 -type f \( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \) | sort)
  else
    "$CLI" -audio "$audio" >> "$LOGS/$name.log" 2>&1; rc=$?
  fi

  secs=$(( $(date +%s) - start ))
  after=$(fabricated "$sidecar")
  printf '%s\trc=%d\t%s\t%s->%s\t%dm\n' "$(date +%H:%M:%S)" "$rc" "$name" "$before" "$after" "$((secs/60))" >> "$PROG"
  echo "$name" >> "$DONE"
done < "$ORDER"
echo "REPAIR_COMPLETE" >> "$PROG"
