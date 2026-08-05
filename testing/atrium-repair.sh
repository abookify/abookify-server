#!/usr/bin/env bash
# Library repair, atrium half: transcribe assigned books on atrium's GPU over
# ssh, pull each sidecar back, and land it on tank — where the database lives.
#
# Split-brain rules this script exists to enforce:
#
# 1. ATRIUM NEVER TOUCHES THE DATABASE. It runs stt-cli against its own copy of
#    the audio and writes a sidecar next to it. Every import/realign runs here
#    on tank via reimport-realign, which serializes with the tank runner's own
#    imports through WAL + busy_timeout. The two runners share no ledger files:
#    this one owns the atrium2-* set. (The one shared write happened at launch:
#    the assigned names were appended to the tank runner's done-list so it skips
#    them — append-only, never rewritten.)
#
# 2. A MISSING FILE IS NOT A COLLAPSED TIMESTAMP. If ssh drops mid-file, the
#    words are simply absent, so the escalation pass — which re-decodes only
#    files still holding collapsed timestamps — would never revisit it, and the
#    book would land partial and read as CLEAN. A pass with a failed file is
#    retried once; if a file still fails, the book records TRANSCRIBE_INCOMPLETE
#    and is NOT landed and NOT recorded done.
#
# 3. TRUST THE INSTRUMENT BEFORE THE OUTPUT. Atrium's whisper container was
#    June-stale at setup; a stale one returns no word confidences, and conf is
#    what text-trust reads. The first sidecar pulled from each book is checked
#    for conf presence; a confless sidecar stalls the run loudly rather than
#    landing books that would all read as low-confidence.
#
# Everything else (per-file resume, done = landed + verified, escalation must
# change decode settings) matches testing/repair-library.sh — see its header.
set -uo pipefail
cd /home/pj/projects/jarvis/abookify/engineering/server
S=/home/pj/tmp/claude-1000/-home-pj-projects-jarvis-abookify-engineering-server/77ca4a16-7ccd-4b76-bfa8-6d948ce25840/scratchpad
ORDER="${ATRIUM_ORDER:-$S/atrium2_order.tsv}"
DONE="$S/atrium2-done.txt"; FILEDONE="$S/atrium2-files-done.txt"
PROG="$S/atrium2-progress.tsv"; BK="$S/repair2-backups"; LOGS="$S/atrium2-logs"
ATTEMPTS="$S/atrium2-attempts.tsv"
RI=./bin/reimport-realign
DB=./data/abookify.db
LIB=./testdata/library
MAXPASS=3
AT=atrium
RPROJ=/home/pj/projects/jarvis/abookify/engineering/server
SSH="ssh -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=4"
mkdir -p "$BK" "$LOGS"; touch "$DONE" "$FILEDONE" "$PROG" "$ATTEMPTS"

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

# conf must be PRESENT and nonzero for a healthy share of words. The writer
# omits conf when it is exactly zero, so a stale whisper (no probabilities)
# yields a sidecar where the key never appears at all.
conf_present() {
  python3 -c "
import json,sys
d=json.load(open(sys.argv[1]))
w=d.get('words',[])
if len(w) < 200: print('ok'); sys.exit()   # too small to judge
have=sum(1 for x in w if x.get('conf',0)>0)
print('ok' if have > len(w)*0.5 else 'CONFLESS')
" "$1" 2>/dev/null || echo CONFLESS
}

# Whisper on atrium must be healthy AND on cuda. A bare compose restart over
# there strips the GPU overlay (the documented gotcha), and a CPU decode at
# 0.8x realtime would silently eat the night.
check_whisper() {
  local h
  h=$($SSH "$AT" 'curl -sf -m 3 http://localhost:5200/health' 2>/dev/null)
  case "$h" in
    *'"device":"cuda"'*) return 0 ;;
    *) return 1 ;;
  esac
}

ensure_whisper() {
  check_whisper && return 0
  printf '%s\tWHISPER_RESTART\n' "$(date +%H:%M:%S)" >> "$PROG"
  $SSH "$AT" "cd '$RPROJ' && docker compose -f docker-compose.yml -f docker-compose.gpu.yml up -d whisper" \
    >> "$LOGS/whisper.log" 2>&1
  local i
  for i in $(seq 1 30); do
    check_whisper && return 0
    sleep 5
  done
  return 1
}

rcli() { $SSH "$AT" "cd '$RPROJ' && bin/stt-cli $*"; }

# Transcribe one pass of one book on atrium. Mirrors repair-library.sh's
# transcribe_pass but over ssh, and returns nonzero if ANY file failed —
# the caller must not land an incomplete pass (rule 2 above).
transcribe_pass() {
  local name="$1" raudio="$2" pass="$3" only="$4" rout="$5" extra=""
  [ "$pass" -ge 2 ] && extra="-no-condition"
  [ "$pass" -ge 3 ] && extra="-no-condition -no-vad"
  local qaudio qout
  qaudio=$(printf %q "$raudio"); qout=$(printf %q "$rout")

  if $SSH "$AT" "[ -d $qaudio ]"; then
    local nfiles
    nfiles=$($SSH "$AT" "find $qaudio -maxdepth 1 -type f \\( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \\) | wc -l")
    if [ "$nfiles" -eq 1 ]; then
      # One-file directory: repair as the file, output pinned to the dir-level
      # path — findSidecar resolves that FIRST, and the default (next to the
      # file) would leave the import landing the OLD decode.
      local rfile qfile
      rfile=$($SSH "$AT" "find $qaudio -maxdepth 1 -type f \\( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \\)")
      qfile=$(printf %q "$rfile")
      rcli "-audio $qfile -output $qout $extra" >> "$LOGS/$name.log" 2>&1
      return $?
    fi

    if ! grep -Fq "$name	p$pass	__bootstrapped__" "$FILEDONE"; then
      if [ "$pass" -eq 1 ]; then
        $SSH "$AT" "rm -f $qout"
        if ! rcli "-audio $qaudio -bootstrap-sidecar" >> "$LOGS/$name.log" 2>&1; then
          return 1
        fi
      fi
      printf '%s\tp%s\t__bootstrapped__\n' "$name" "$pass" >> "$FILEDONE"
    fi
    local rc=0 list
    if [ -n "$only" ]; then
      list="$only"
    else
      list=$($SSH "$AT" "find $qaudio -maxdepth 1 -type f \\( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \\) -printf '%f\\n' | sort")
    fi
    while IFS= read -r fn; do
      [ -z "$fn" ] && continue
      grep -Fq "$name	p$pass	$fn" "$FILEDONE" && continue
      local qfn ok=0 try
      qfn=$(printf %q "$fn")
      for try in 1 2; do
        if rcli "-audio $qaudio -redo-files $qfn $extra" >> "$LOGS/$name.log" 2>&1; then
          ok=1; break
        fi
        printf '%s\tFILE_FAIL\t%s\tp%s\t%s\ttry%s\n' "$(date +%H:%M:%S)" "$name" "$pass" "$fn" "$try" >> "$PROG"
        ensure_whisper || true
      done
      if [ "$ok" -eq 1 ]; then
        printf '%s\tp%s\t%s\n' "$name" "$pass" "$fn" >> "$FILEDONE"
      else
        rc=1
      fi
    done <<< "$list"
    return $rc
  fi

  # Single file (never inside a directory here — those were resolved above).
  rcli "-audio $qaudio -output $qout $extra" >> "$LOGS/$name.log" 2>&1
}

pull_sidecar() {
  local rsidecar="$1" local_sc="$2"
  rsync -a --timeout=60 "$AT:$rsidecar" "$local_sc"
}

CONF_CHECKED=0

while IFS=$'\t' read -r fab dur sidecar raudio workid; do
  case "$fab" in ''|\#*) continue;; esac
  name=$(basename "$sidecar" .stt.json)
  grep -Fxq "$name" "$DONE" && continue

  # Remote sidecar path: dir input -> "<dir>.stt.json"; file input -> ext
  # swapped for .stt.json. One-file dirs are pinned to the dir-level path.
  case "$raudio" in
    *.mp3|*.m4a|*.m4b|*.flac|*.wav|*.ogg|*.opus) rsidecar="${raudio%.*}.stt.json" ;;
    *) rsidecar="${raudio}.stt.json" ;;
  esac

  if ! ensure_whisper; then
    printf '%s\tRUNNER_STALL\twhisper_unhealthy_on_atrium\n' "$(date +%H:%M:%S)" >> "$PROG"
    exit 1
  fi

  [ -f "$BK/$name.stt.json" ] || cp "$sidecar" "$BK/$name.stt.json" 2>/dev/null
  before=$(collapsed "$BK/$name.stt.json")
  start=$(date +%s)
  verdict="" ; used=0 ; after="$before"

  # Escalation scope: multi-file books re-decode only residual-damaged files;
  # single files (and one-file directories) escalate over the whole book, since
  # an empty residual list there just means the sidecar has no sources array.
  qra=$(printf %q "$raudio")
  is_multi=0
  if $SSH "$AT" "[ -d $qra ]"; then
    nf=$($SSH "$AT" "find $qra -maxdepth 1 -type f \\( -name '*.mp3' -o -name '*.m4a' -o -name '*.m4b' -o -name '*.flac' \\) | wc -l")
    [ "$nf" -gt 1 ] && is_multi=1
  fi

  for pass in $(seq 1 $MAXPASS); do
    only=""
    if [ "$pass" -gt 1 ]; then
      only=$(residual_files "$sidecar")
      [ -z "$only" ] && [ "$is_multi" -eq 1 ] && break
    fi
    used=$pass
    if ! transcribe_pass "$name" "$raudio" "$pass" "$only" "$rsidecar"; then
      verdict="TRANSCRIBE_INCOMPLETE"
      printf '%s\tpass%s\t%s\tTRANSCRIBE_INCOMPLETE\tnot_landed\n' "$(date +%H:%M:%S)" "$pass" "$name" >> "$PROG"
      break
    fi
    if ! pull_sidecar "$rsidecar" "$sidecar"; then
      verdict="PULL_FAILED"
      printf '%s\tpass%s\t%s\tPULL_FAILED\tnot_landed\n' "$(date +%H:%M:%S)" "$pass" "$name" >> "$PROG"
      break
    fi

    if [ "$CONF_CHECKED" -eq 0 ]; then
      if [ "$(conf_present "$sidecar")" != "ok" ]; then
        printf '%s\tRUNNER_STALL\tSCHEMA_STALE_no_conf_in_sidecar_rebuild_whisper_on_atrium\n' "$(date +%H:%M:%S)" >> "$PROG"
        exit 1
      fi
      CONF_CHECKED=1
    fi

    n=$(grep -c "^$name	" "$ATTEMPTS" 2>/dev/null)
    n=$(( ${n:-0} + 1 ))
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
    LAND_RETRY_PENDING|TRANSCRIBE_INCOMPLETE|PULL_FAILED) : ;;  # not done — a later run retries
    *) echo "$name" >> "$DONE" ;;
  esac
done < "$ORDER"

want=$(grep -cv '^\s*$\|^#' "$ORDER")
have=$(sort -u "$DONE" | grep -c .)
if [ "$have" -ge "$want" ]; then
  echo "ATRIUM_REPAIR_COMPLETE" >> "$PROG"
else
  printf 'ATRIUM_REPAIR_EXITED_EARLY\t%s_of_%s_books_recorded\n' "$have" "$want" >> "$PROG"
fi
