#!/usr/bin/env bash
# STT GPU-vs-CPU benchmark. Model already cached in ~/.abookify/models/whisper.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE="$HERE/dist/engine"
AUDIO="${ABOOKIFY_TEST_AUDIO:-$HERE/../testdata/transcription-experiments/kitchen-confidential/runs/slice_01_0-600.mp3}"
OUT="$HERE/.validate"; mkdir -p "$OUT"

run_one() {
  local dev="$1" comp="$2" label="$3"
  echo ""; echo "=== STT on $label ($dev/$comp) ==="
  ABOOKIFY_DEVICE="$dev" ABOOKIFY_COMPUTE_TYPE="$comp" \
    "$BUNDLE/abookify-engine" --stt-only > "$OUT/stt_$label.log" 2>&1 &
  local pid=$!
  for i in $(seq 1 90); do
    curl -fsS http://127.0.0.1:5200/health >/dev/null 2>&1 && break
    sleep 2
  done
  if ! curl -fsS http://127.0.0.1:5200/health >/dev/null 2>&1; then
    echo "  STT failed to come up; log:"; tail -5 "$OUT/stt_$label.log"; kill $pid 2>/dev/null; return 1
  fi
  echo -n "  /health: "; curl -fsS http://127.0.0.1:5200/health; echo
  local t0 t1; t0=$(date +%s.%N)
  curl -fsS -X POST http://127.0.0.1:5200/transcribe -F "file=@$AUDIO" -F "word_timestamps=true" -o "$OUT/stt_$label.json"
  t1=$(date +%s.%N)
  "$BUNDLE/python/bin/python3" - "$OUT/stt_$label.json" "$t0" "$t1" <<'PY'
import json, sys
d=json.load(open(sys.argv[1])); el=float(sys.argv[3])-float(sys.argv[2])
dur=d.get("duration",0); words=sum(len(s.get("words",[])) for s in d.get("segments",[]))
print(f"  audio {dur:.0f}s -> {words} words in {el:.1f}s wall = {dur/el:.1f}x realtime")
print("  text[:140]:", d.get("text","")[:140])
PY
  kill $pid 2>/dev/null; wait $pid 2>/dev/null
}

echo "audio: $(basename "$AUDIO")"
run_one cuda float16 gpu
run_one cpu  int8    cpu
echo ""; echo "bench done."
