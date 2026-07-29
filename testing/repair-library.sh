#!/usr/bin/env bash
# Library repair: re-transcribe books carrying fabricated text, worst first.
#
# Worst-first so the biggest correctness win lands earliest and the run can be
# stopped at any point with the remainder being the least affected books.
#
# Resumable: a book whose name is in done.txt is skipped, so an interrupted run
# continues rather than restarting 60 hours of work.
set -uo pipefail
cd /home/pj/projects/jarvis/abookify/engineering/server
S=/home/pj/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad
ORDER="$S/repair_order.tsv"; DONE="$S/repair-done.txt"; PROG="$S/repair-progress.tsv"
touch "$DONE" "$PROG"

fabricated() {  # count words sharing a timestamp with >6 others
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

  # Resolve the audio: a directory, or a single file beside the sidecar.
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
    echo "$name" >> "$DONE"
    continue
  fi

  before=$(fabricated "$sidecar")
  cp "$sidecar" "$S/repair-backups/$name.stt.json" 2>/dev/null
  start=$(date +%s)
  ../server-transcription/bin/stt-cli -audio "$audio" > "$S/repair-logs/$name.log" 2>&1
  rc=$?
  secs=$(( $(date +%s) - start ))
  after=$(fabricated "$sidecar")
  printf '%s\trc=%d\t%s\t%s->%s\t%dm\n' "$(date +%H:%M:%S)" "$rc" "$name" "$before" "$after" "$((secs/60))" >> "$PROG"
  echo "$name" >> "$DONE"
done < "$ORDER"
echo "REPAIR_COMPLETE" >> "$PROG"
