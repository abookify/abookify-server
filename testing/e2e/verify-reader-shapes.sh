#!/usr/bin/env bash
# Reader verification across the THREE structural shapes — run this for ANY
# reader/nav/sync change, not a single hand-picked work.
#
# WHY THIS EXISTS (the lesson, 2026-08-10): the human-default means every live
# multi-edition work exercises the TRANSCRIPT follow path. Only a TTS-ONLY work
# walks the ebook-word-karaoke path — and a TTS-only work is exactly the clean
# showcase sample AND every book a GPU-less user generates with Kokoro. Verifying
# 85/76/9 (all transcript-path) passed honestly while the ebook-karaoke coupling
# was broken, because none of them reach it. A suite that structurally cannot
# reach the shipping configuration is not a passing suite. So: ALWAYS include a
# TTS-only work. This script discovers one of each shape and refuses to run if it
# cannot find a TTS-only work — the path must never go unverified again.
#
# Usage: verify-reader-shapes.sh <base_url> [session_cookie]
#   e.g. verify-reader-shapes.sh http://localhost:7654 dev-mobile-screenshots-9271
set -euo pipefail
BASE="${1:?usage: verify-reader-shapes.sh <base_url> [cookie]}"
COOKIE="${2:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"

AUTH=()
[ -n "$COOKIE" ] && AUTH=(-H "Authorization: Bearer $COOKIE")

# Classify every work into one of the three shapes and pick the first of each.
# tts_only  : all audio origin tts_kokoro, no transcript text (the ebook-karaoke path)
# ebook_kar : has a human/narrator audio + an epub (transcript-backed ebook-karaoke)
# transcript: has a whisper_transcript text source
picks=$(curl -s -m20 "${AUTH[@]}" "$BASE/api/works" | python3 -c '
import sys,json
ws=json.load(sys.stdin)
tts_only=ebook=transcript=None
for w in ws:
    au=[b.get("origin") for b in (w.get("audio_files") or [])]
    tx=[b.get("origin") or b.get("format") for b in (w.get("text_files") or [])]
    has_epub=any("epub" in str(b.get("format")) for b in (w.get("text_files") or []))
    has_trans=any("transcript" in str(o) for o in tx)
    if au and all(o=="tts_kokoro" for o in au) and not has_trans and has_epub and tts_only is None:
        tts_only=w["id"]
    if any(o in ("narrator_recording","author_recording","librivox") for o in au) and has_epub and ebook is None:
        ebook=w["id"]
    if has_trans and transcript is None:
        transcript=w["id"]
print(json.dumps({"tts_only":tts_only,"ebook":ebook,"transcript":transcript}))
')
tts_only=$(echo "$picks" | python3 -c 'import sys,json;print(json.load(sys.stdin)["tts_only"] or "")')
ebook=$(echo    "$picks" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ebook"] or "")')
transcript=$(echo "$picks" | python3 -c 'import sys,json;print(json.load(sys.stdin)["transcript"] or "")')

echo "shapes discovered: tts_only=$tts_only  ebook(human)=$ebook  transcript=$transcript"
if [ -z "$tts_only" ]; then
  echo "FATAL: no TTS-only work found — the ebook-karaoke path CANNOT be verified on this server."
  echo "Add a TTS-generated (Kokoro) book with an epub, or import a showcase sample, then re-run."
  exit 2
fi

rc=0
for pair in "tts_only:$tts_only" "ebook:$ebook" "transcript:$transcript"; do
  shape="${pair%%:*}"; wid="${pair#*:}"
  [ -z "$wid" ] && { echo "== $shape: none on this server (skipped) =="; continue; }
  echo "======== $shape → work $wid ========"
  args=(--base "$BASE" --work "$wid")
  [ -n "$COOKIE" ] && args+=(--cookie "$COOKIE")
  if node "$HERE/run-web.js" "${args[@]}" | grep -E '^PASS|^FAIL|journeys passed'; then
    [ "${PIPESTATUS[0]}" -eq 0 ] || rc=1
  else
    rc=1
  fi
done
[ "$rc" -eq 0 ] && echo "ALL SHAPES GREEN" || echo "SOME SHAPE RED (rc=$rc)"
exit $rc
