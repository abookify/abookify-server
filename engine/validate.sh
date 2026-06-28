#!/usr/bin/env bash
# Smoke-test + benchmark the built bundle. Starts the engine, hits both
# endpoints, transcribes a 10-min clip, synthesizes a sentence. Requires
# dist/engine to exist (run build.sh first).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE="${1:-$HERE/dist/engine}"
AUDIO="${ABOOKIFY_TEST_AUDIO:-$HERE/../testdata/transcription-experiments/kitchen-confidential/runs/slice_01_0-600.mp3}"
OUT="$HERE/.validate"; mkdir -p "$OUT"

PY="$BUNDLE/python/bin/python3"
[ -x "$PY" ] || { echo "no bundle at $BUNDLE — run build.sh first" >&2; exit 1; }

echo "=== device probe (in-bundle) ==="
"$BUNDLE/python/bin/python3" - <<'PY'
import ctranslate2, torch
print("ctranslate2 cuda devices:", ctranslate2.get_cuda_device_count())
print("torch.cuda.is_available:", torch.cuda.is_available())
if torch.cuda.is_available():
    print("torch gpu:", torch.cuda.get_device_name(0))
PY

echo ""; echo "=== launching engine (background) ==="
"$BUNDLE/abookify-engine" > "$OUT/engine.log" 2>&1 &
ENGINE_PID=$!
trap 'kill $ENGINE_PID 2>/dev/null' EXIT

# wait for both health endpoints. First run downloads large-v3 (~3GB) + kokoro
# weights before /health responds, which can take many minutes on a throttled
# HF connection — hence the generous default. Override with ABOOKIFY_HEALTH_WAIT
# (seconds). Pre-seed ~/.abookify/models/ to make this fast.
WAIT="${ABOOKIFY_HEALTH_WAIT:-1800}"
for url in http://127.0.0.1:5200/health http://127.0.0.1:8880/health; do
  echo -n "waiting $url (up to ${WAIT}s for cold model download) "
  ok=0
  for i in $(seq 1 $((WAIT/2))); do
    if curl -fsS "$url" >/dev/null 2>&1; then echo " OK"; ok=1; break; fi
    sleep 2; echo -n "."
  done
  [ "$ok" = 1 ] || echo " TIMED OUT (model still downloading? check engine log)"
done
echo "--- STT /health:"; curl -fsS http://127.0.0.1:5200/health; echo
echo "--- TTS /v1/models:"; curl -fsS http://127.0.0.1:8880/v1/models; echo

echo ""; echo "=== STT benchmark: $(basename "$AUDIO") ==="
if [ -f "$AUDIO" ]; then
  t0=$(date +%s.%N)
  curl -fsS -X POST http://127.0.0.1:5200/transcribe -F "file=@$AUDIO" -F "word_timestamps=true" -o "$OUT/stt.json"
  t1=$(date +%s.%N)
  "$PY" - "$OUT/stt.json" "$t0" "$t1" <<'PY'
import json, sys
d=json.load(open(sys.argv[1])); el=float(sys.argv[3])-float(sys.argv[2])
dur=d.get("duration",0); n=len(d.get("segments",[]))
words=sum(len(s.get("words",[])) for s in d.get("segments",[]))
print(f"audio {dur:.0f}s -> {n} segs, {words} words in {el:.1f}s wall ({dur/el:.1f}x realtime)")
print("first words:", d.get("text","")[:160])
PY
else
  echo "(no test audio at $AUDIO — skipping STT)"
fi

echo ""; echo "=== TTS synth ==="
t0=$(date +%s.%N)
curl -fsS -X POST http://127.0.0.1:8880/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"model":"kokoro","input":"The Nellie, a cruising yawl, swung to her anchor without a flutter of the sails, and was at rest.","voice":"af_heart","response_format":"mp3"}' \
  -o "$OUT/tts.mp3"
t1=$(date +%s.%N)
echo "wrote $OUT/tts.mp3 ($(stat -c%s "$OUT/tts.mp3" 2>/dev/null || echo 0) bytes) in $(echo "$t1-$t0"|bc 2>/dev/null || echo ?)s"
file "$OUT/tts.mp3" 2>/dev/null || true

echo ""; echo "=== engine log tail ==="; tail -8 "$OUT/engine.log"
echo ""; echo "validation done."
