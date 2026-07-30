#!/usr/bin/env bash
# Library repair: re-transcribe books carrying fabricated text, worst first,
# landing each one in the database and escalating decode settings until the book
# is actually clean.
#
# Three rules this script exists to enforce, each of them learned by getting it
# wrong first:
#
# 1. RESUME IS PER FILE, NOT PER BOOK. Recording a book done only once every file
#    finished meant a crash 7 h into Atlas Shrugged (53 h of audio) discarded all
#    7 hours. Multi-file books are bootstrapped first (--bootstrap-sidecar writes
#    sources + durations with no words) then filled one file at a time with
#    --redo-files, recording each file. An interruption costs the file in flight
#    (~10-30 min), not the book.
#
# 2. DONE MEANS LANDED AND VERIFIED, NOT "THE COMMAND EXITED". stt-cli writes a
#    sidecar file; the reader, search and Q&A read the database. A run that only
#    rewrote sidecars would finish perfectly and change nothing a user sees. Worse,
#    rewriting the text invalidates the alignment describing it, so a book can end
#    up with new text and an alignment pointing at text that no longer exists —
#    invisible, and indistinguishable to a reader from karaoke being broken. So the
#    book is recorded done only when reimport-realign exits 0, and that command
#    verifies alignment freshness itself.
#
# 3. A SECOND PASS MUST DIFFER FROM THE FIRST. The pipeline is deterministic —
#    segments are cut with `ffmpeg -c copy` — so re-running an identical decode
#    reproduces the identical fabricated text. Escalation therefore changes decode
#    settings rather than repeating: pass 2 disables conditioning on previous text
#    (the direct remedy for repetition loops, which is what the survivors are), and
#    pass 3 also disables the voice-activity filter. A book is clean when it has no
#    collapsed timestamps left; if it still does after pass 3, that is recorded as
#    its floor WITH the reason rather than being reported as success.
set -uo pipefail
cd /home/pj/projects/jarvis/abookify/engineering/server
S=/home/pj/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad
ORDER="${REPAIR_ORDER:-$S/repair2_order.tsv}"
DONE="$S/repair2-done.txt"; FILEDONE="$S/repair2-files-done.txt"
PROG="$S/repair2-progress.tsv"; BK="$S/repair2-backups"; LOGS="$S/repair2-logs"
ATTEMPTS="$S/repair2-attempts.tsv"
CLI=../server-transcription/bin/stt-cli
RI=./bin/reimport-realign
DB=./data/abookify.db
LIB=./testdata/library
MAXPASS=3
mkdir -p "$BK" "$LOGS"; touch "$DONE" "$FILEDONE" "$PROG" "$ATTEMPTS"

# Collapsed-timestamp words in a sidecar: the fabrication signature.
collapsed() {
  python3 -c "
import json,sys
from collections import Counter
try: d=json.load(open(sys.argv[1]))
except Exception: print(-1); sys.exit()
c=Counter(x['s'] for x in d.get('words',[]))
print(sum(v for v in c.values() if v>6))
" "$1" 2>/dev/null || echo -1
}

# Which source files still contain collapsed timestamps — so an escalated pass
# re-decodes only the damaged files instead of the whole book.
residual_files() {
  python3 -c "
import json,sys
from collections import Counter
d=json.load(open(sys.argv[1]))
srcs=d.get('sources',[])
bad=[t for t,n in Counter(x['s'] for x in d.get('words',[])).items() if n>6]
if not srcs:
    sys.exit()
out=[]
for t in bad:
    for s in srcs:
        if s['start_sec'] <= t < s['start_sec']+s['duration']:
            out.append(s['filename']); break
for f in sorted(set(out)): print(f)
" "$1" 2>/dev/null
}

# Transcribe one book at a given pass. Pass 1 is plain; later passes escalate.
# Multi-file books go file by file so progress survives an interruption.
transcribe_pass() {
  local name="$1" audio="$2" pass="$3" only="$4" extra=""
  [ "$pass" -ge 2 ] && extra="-no-condition"
  [ "$pass" -ge 3 ] && extra="-no-condition -no-vad"

  if [ -d "$audio" ]; then
    if ! grep -Fq "$name	p$pass	__bootstrapped__" "$FILEDONE"; then
      if [ "$pass" -eq 1 ]; then
        rm -f "${audio}.stt.json"
        if ! $CLI -audio "$audio" -bootstrap-sidecar >> "$LOGS/$name.log" 2>&1; then
          return 1
        fi
      fi
      printf '%s\tp%s\t__bootstrapped__\n' "$name" "$pass" >> "$FILEDONE"
    fi
    local rc=0 list
    if [ -n "$only" ]; then
      list="$only"
    else
      list=$(find "$audio" -maxdepth 1 -type f \
        \( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \) | sort)
    fi
    while IFS= read -r f; do
      [ -z "$f" ] && continue
      local fn; fn=$(basename "$f")
      grep -Fq "$name	p$pass	$fn" "$FILEDONE" && continue
      if $CLI -audio "$audio" -redo-files "$fn" $extra >> "$LOGS/$name.log" 2>&1; then
        printf '%s\tp%s\t%s\n' "$name" "$pass" "$fn" >> "$FILEDONE"
      else
        rc=1
        printf '%s\tFILE_FAIL\t%s\tp%s\t%s\n' "$(date +%H:%M:%S)" "$name" "$pass" "$fn" >> "$PROG"
      fi
    done <<< "$list"
    return $rc
  fi

  # Single file: no per-file resume to be had, the file IS the book.
  $CLI -audio "$audio" $extra >> "$LOGS/$name.log" 2>&1
}

while IFS=$'\t' read -r fab dur sidecar audiodir workid; do
  case "$fab" in ''|\#*) continue;; esac
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
    printf '%s\tORPHAN\t%s\t-\t-\t-\n' "$(date +%H:%M:%S)" "$name" >> "$PROG"
    echo "$name" >> "$DONE"; continue
  fi

  # Measure "before" from the BACKUP. On a resumed book the live sidecar is already
  # partially rebuilt, so reading it there reports a before-figure from the partial
  # state — Free Will resumed and reported "0->0" when the truth was 516 -> 0.
  [ -f "$BK/$name.stt.json" ] || cp "$sidecar" "$BK/$name.stt.json" 2>/dev/null
  before=$(collapsed "$BK/$name.stt.json")
  start=$(date +%s)
  verdict="" ; used=0 ; after="$before"

  for pass in $(seq 1 $MAXPASS); do
    only=""
    if [ "$pass" -gt 1 ]; then
      only=$(residual_files "$sidecar")
      # Single-file book, or sources missing: escalate over the whole book.
      [ -z "$only" ] && [ -d "$audio" ] && break
    fi
    used=$pass
    transcribe_pass "$name" "$audio" "$pass" "$only"

    # Land it before judging: the database is what the product reads, and
    # reimport-realign verifies the alignment was rewritten for the new text.
    n=$(( $(grep -c "^$name	" "$ATTEMPTS" 2>/dev/null || echo 0) + 1 ))
    printf '%s\tp%s\n' "$name" "$pass" >> "$ATTEMPTS"
    if [ -n "$workid" ] && [ "$workid" != "0" ]; then
      if $RI -db "$DB" -library "$LIB" -work "$workid" >> "$LOGS/$name.log" 2>&1; then
        land="landed"
      else
        land="LAND_FAIL"
      fi
    else
      land="NO_WORK_ID"
    fi

    after=$(collapsed "$sidecar")
    printf '%s\tpass%s\t%s\t%s->%s\t%s\n' "$(date +%H:%M:%S)" "$pass" "$name" "$before" "$after" "$land" >> "$PROG"

    if [ "$land" != "landed" ]; then
      # Not landed = not done. Leave it unrecorded so a later run retries, unless
      # it has already failed twice, in which case record the failure and move on
      # rather than blocking the remaining books forever.
      if [ "$n" -ge 2 ]; then
        verdict="LAND_FAILED_TWICE"
        break
      fi
      verdict="LAND_RETRY_PENDING"
      break
    fi

    if [ "$after" = "0" ]; then verdict="CLEAN"; break; fi
    [ "$pass" -eq "$MAXPASS" ] && verdict="FLOOR_${after}_after_${MAXPASS}_passes"
  done

  secs=$(( $(date +%s) - start ))
  printf '%s\tBOOK\t%s\t%s->%s\tpasses=%s\t%dm\t%s\n' \
    "$(date +%H:%M:%S)" "$name" "$before" "$after" "$used" "$((secs/60))" "${verdict:-UNKNOWN}" >> "$PROG"

  case "$verdict" in
    LAND_RETRY_PENDING) : ;;                 # deliberately NOT recorded done
    *) echo "$name" >> "$DONE" ;;
  esac
done < "$ORDER"
echo "REPAIR_COMPLETE" >> "$PROG"
