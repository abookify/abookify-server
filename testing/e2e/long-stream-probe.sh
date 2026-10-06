#!/usr/bin/env bash
# long-stream-probe.sh — does a relayed audio stream survive the relay's 10-minute connection cut?
# (mobile lane, 2026-10-06; see engineering/mobile/abookify-mobile/docs/e2e-pinning-assessment.md)
#
# Samples the phone's media session + the app's rendered position every INTERVAL seconds for
# DURATION seconds while a book plays, so the 10-minute mark is bracketed by readings on both
# sides. A stall shows as PAUSED/BUFFERING with a frozen position; a restart shows the position
# dropping to the chapter start; a lost position shows the app's position disagreeing with the
# session clock. Reads only; starts nothing. Pair the app to the RELAYED hostname first (not adb
# reverse — the point is the relay), start playback, then run this.
#
#   DURATION=780 INTERVAL=30 testing/e2e/long-stream-probe.sh | tee /tmp/long-stream.log
#
# The app's own position is read from the E2E probe line when the NowPlaying/Reader screen is
# showing (release builds without EXPO_PUBLIC_E2E have no probe — the media session alone still
# answers stall/restart; only "lost position" needs the probe or a GET /api/works/{id}/position
# afterwards).
set -u
DURATION="${DURATION:-780}"; INTERVAL="${INTERVAL:-30}"; PKG=com.abookify.app
t0=$(date +%s)
printf '%-6s %-10s %-10s %-10s %-6s %s\n' secs state pos_s buf_s speed app_pos
while :; do
  now=$(date +%s); el=$((now - t0)); [ "$el" -gt "$DURATION" ] && break
  ms=$(adb shell dumpsys media_session 2>/dev/null | tr -d '\r' | awk -v p="$PKG" 'index($0,p){f=1} f && /state=PlaybackState/{print; exit}')
  st=$(echo "$ms" | grep -oE 'state=[A-Z_]+' | head -1 | cut -d= -f2)
  pos=$(echo "$ms" | grep -oE 'position=-?[0-9]+' | head -1 | cut -d= -f2)
  buf=$(echo "$ms" | grep -oE 'buffered position=-?[0-9]+' | head -1 | awk -F= '{print $2}')
  sp=$(echo "$ms" | grep -oE 'speed=[-0-9.]+' | head -1 | cut -d= -f2)
  app=$(adb shell uiautomator dump /sdcard/lsp.xml >/dev/null 2>&1 && adb shell cat /sdcard/lsp.xml 2>/dev/null | grep -oE 'E2E\{[^}]*\}' | head -1 | grep -oE '"pos":[0-9.]+' | cut -d: -f2)
  printf '%-6s %-10s %-10s %-10s %-6s %s\n' "$el" "${st:-?}" "$(awk -v v="${pos:-0}" 'BEGIN{printf "%.1f", v/1000}')" "$(awk -v v="${buf:-0}" 'BEGIN{printf "%.1f", v/1000}')" "${sp:-?}" "${app:--}"
  sleep "$INTERVAL"
done
