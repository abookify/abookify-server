#!/usr/bin/env bash
# drop-probe.sh — what does the phone do when the connection to the server DROPS mid-chapter?
# (mobile lane, 2026-10-06 — meta: "test the drop, not the timeout")
#
# While a streamed book is PLAYING on the emulator, cut the path to the server for DROP seconds,
# restore it, and keep sampling the media session (state / position / buffered / speed) every
# STEP seconds. Two cut modes:
#   MODE=tunnel   remove `adb reverse tcp:PORT` — closes every forwarded TCP socket at once,
#                 exactly what a relay connection close does to the phone; the device's own
#                 network stays up.
#   MODE=radios   `svc wifi disable; svc data disable` for DROP s (restore re-enables Wi-Fi only if it was on) — the real dead spot on a RELAY path (throttle: `adb emu network speed edge`).
#   MODE=radio    airplane mode on/off — the dead-spot case. NOTE (2026-10-06): when the app reaches the
#                 server over `adb reverse` (loopback), airplane mode does NOT cut the stream — use MODE=adb,
#                 or a real relayed/LAN address, for a true cut.
# Reads the result: a STALL = PAUSED/BUFFERING with a frozen position past the restore; a RESTART =
# position drops to ≈0/chapter start; a LOST POSITION = the position after resume is not continuous
# with the position before the cut (allowing for the drop itself); CLEAN = PLAYING resumes and the
# position continues within a few seconds of restore.
#
#   PORT=7654 MODE=tunnel DROP=30 PRE=20 POST=90 STEP=5 testing/e2e/drop-probe.sh
set -u
PORT="${PORT:-7654}"; MODE="${MODE:-tunnel}"; DROP="${DROP:-30}"; PRE="${PRE:-20}"; POST="${POST:-90}"; STEP="${STEP:-5}"
PKG=com.abookify.app
sample() {
  # Read the app's session line AND the device uptime in ONE shell so the clocks agree. The dump's
  # `position` is stamped only at player events (a play/pause flip, a buffering cycle); while PLAYING
  # the live position = position + (now − updated) × speed, exactly as the lock screen computes it.
  local out ms st pos buf sp upd now live
  out=$(adb shell "dumpsys media_session; echo __UP__; cat /proc/uptime" 2>/dev/null | tr -d '\r')
  ms=$(echo "$out" | awk -v p="$PKG" 'index($0,p){f=1} f && /state=PlaybackState/{print; exit}')
  st=$(echo "$ms" | grep -oE 'PlaybackState \{state=[A-Z_]+' | head -1 | sed 's/.*state=//')
  pos=$(echo "$ms" | grep -oE 'position=-?[0-9]+' | head -1 | cut -d= -f2)
  buf=$(echo "$ms" | grep -oE 'buffered position=-?[0-9]+' | head -1 | awk -F= '{print $2}')
  sp=$(echo "$ms" | grep -oE 'speed=[-0-9.]+' | head -1 | cut -d= -f2)
  upd=$(echo "$ms" | grep -oE 'updated=[0-9]+' | head -1 | cut -d= -f2)
  now=$(echo "$out" | awk '/__UP__/{getline; print $1*1000; exit}')
  live=$(awk -v p="${pos:-0}" -v u="${upd:-0}" -v n="${now:-0}" -v s="${sp:-0}" -v st="$st" 'BEGIN{ if (st=="PLAYING" && n>=u && u>0) printf "%.1f", (p + (n-u)*s)/1000; else printf "%.1f", p/1000 }')
  printf '%-5s %-7s %-10s %-9s %-9s %-9s %s\n' "$1" "$2" "${st:-?}" "$live" "$(awk -v v="${pos:-0}" 'BEGIN{printf "%.1f", v/1000}')" "$(awk -v v="${buf:-0}" 'BEGIN{printf "%.1f", v/1000}')" "${sp:-?}"
}
# MODE=tunnel (adb reverse --remove) does NOT close an already-open streaming socket — a 2026-10-06 run
# sailed through a 180 s "cut" with the position advancing in step with wall time. It is kept only for
# "no NEW connections". MODE=adb kills the adb transport, which resets every forwarded socket at once
# (the FIN/RST a relay close delivers); the device cannot be sampled while adb is down, so that mode
# samples before and after only — the after phase tells the story.
cut_on()  { case "$MODE" in radio) adb shell cmd connectivity airplane-mode enable;; radios) WIFI_WAS=$(adb shell dumpsys wifi 2>/dev/null | grep -c "Wi-Fi is enabled"); adb shell "svc wifi disable; svc data disable";; adb) adb kill-server;; *) adb reverse --remove tcp:$PORT;; esac; }
cut_off() { case "$MODE" in radios) adb shell "svc data enable"; [ "${WIFI_WAS:-0}" -gt 0 ] && adb shell svc wifi enable;; radio) adb shell cmd connectivity airplane-mode disable;; adb) adb start-server >/dev/null 2>&1; sleep 2; adb wait-for-device; adb reverse tcp:$PORT tcp:$PORT >/dev/null;; *) adb reverse tcp:$PORT tcp:$PORT >/dev/null;; esac; }
printf '%-5s %-7s %-10s %-9s %-9s %-9s %s\n' secs phase state live_s raw_s buf_s speed
t0=$(date +%s); el() { echo $(( $(date +%s) - t0 )); }
while [ "$(el)" -lt "$PRE" ]; do sample "$(el)" before; sleep "$STEP"; done
cut_on; echo "--- CUT ($MODE) for ${DROP}s at $(el)s"
tc=$(date +%s); while [ $(( $(date +%s) - tc )) -lt "$DROP" ]; do if [ "$MODE" = adb ]; then sleep "$STEP"; else sample "$(el)" CUT; sleep "$STEP"; fi; done
cut_off; echo "--- RESTORED at $(el)s"
tr=$(date +%s); while [ $(( $(date +%s) - tr )) -lt "$POST" ]; do sample "$(el)" after; sleep "$STEP"; done
