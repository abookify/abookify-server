#!/usr/bin/env bash
# repair-mp3.sh — rebuild MP3 framing so a decoder can read PAST corrupt frames.
#
# Damaged frames do not merely glitch: ffmpeg (and therefore Whisper) stops
# producing output for the REST OF THE STREAM after hitting them. Life of Pi's
# 18 "transcription gaps" were entirely this — 11 of 11 gap-bearing files carried
# decode errors, and re-transcribing them recovered 17 words because the decoder
# died at the same place every time. Repairing one file recovered 600 words from
# a single 10-minute chunk.
#
# Seeking past the damage works (which is why spot-checks mislead), so the fix is
# to re-encode: bad frames are skipped, framing is rebuilt, everything after them
# becomes reachable.
#
# Lossy-on-lossy, deliberately. The alternative is losing the audio entirely, and
# -q:a 4 (~165 kbps VBR) is above these 128 kbps sources. Originals are NOT
# touched — repair writes beside the file and swaps only after verifying.
#
#   bin/repair-mp3.sh <file.mp3> [...]        repair in place (backup kept)
#   bin/repair-mp3.sh --check <file.mp3> [...] report damage only
set -uo pipefail

check_only=0
[ "${1:-}" = "--check" ] && { check_only=1; shift; }

errs() { ffmpeg -v error -i "$1" -f null - 2>&1 | grep -c 'Header missing\|Invalid data' || true; }

rc=0
for f in "$@"; do
  [ -f "$f" ] || { echo "  MISSING  $f"; rc=1; continue; }
  before=$(errs "$f")
  if [ "$before" -eq 0 ]; then
    echo "  clean    $f"
    continue
  fi
  if [ "$check_only" -eq 1 ]; then
    echo "  DAMAGED  $f ($before decode errors)"
    rc=1
    continue
  fi

  tmp="${f%.mp3}.repair.$$.mp3"
  if ! ffmpeg -y -v error -err_detect ignore_err -i "$f" -c:a libmp3lame -q:a 4 "$tmp" 2>/dev/null; then
    echo "  FAILED   $f (re-encode error)"; rm -f "$tmp"; rc=1; continue
  fi
  after=$(errs "$tmp")
  # Duration must survive: a repair that truncates the book is worse than the
  # damage it fixes.
  d1=$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$f")
  d2=$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$tmp")
  if awk -v a="$d1" -v b="$d2" 'BEGIN{exit !(b < a-2 || b > a+2)}'; then
    echo "  REJECTED $f (duration $d1 -> $d2, refusing to swap)"; rm -f "$tmp"; rc=1; continue
  fi
  if [ "$after" -ne 0 ]; then
    echo "  PARTIAL  $f ($before -> $after errors; swapping anyway)"
  fi
  cp -n "$f" "${f}.orig" 2>/dev/null || true   # keep the first original only
  mv -f "$tmp" "$f"
  echo "  repaired $f ($before -> $after errors, ${d1%.*}s preserved)"
done
exit $rc
