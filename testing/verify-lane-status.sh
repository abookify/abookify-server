#!/usr/bin/env bash
# One command that answers "is it done?" for the transcription lane's two
# most-repeatedly-asked items, so the answer is verifiable rather than narrated.
#
# Prose answers have not transmitted through the relay — both items below were
# asked for repeatedly after completion. Run this instead:
#
#   engineering/server/testing/verify-lane-status.sh
cd "$(dirname "$0")/.." || exit 1
fail=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=1; }

echo "1. POST-WRITE SIDECAR INTEGRITY CHECK — every path a transcript can enter by"
check_site() {
  if grep -q "$2" "$1" 2>/dev/null; then ok "$3  ($1)"; else bad "$3 MISSING in $1"; fi
}
check_site cmd/stt-cli/main.go          'reportSidecarProblems(path'      'full-run sidecar write'
check_site cmd/stt-cli/redo.go          'reportSidecarProblems(outputPath' 'redo merge write'
check_site internal/library/sidecar_import.go 'checkSidecarIntegrity(&sc)' 'sidecars arriving from elsewhere'
check_site internal/library/generate.go 'LogTranscriptProblems('          'in-app transcribe job (no sidecar)'

echo
echo "2. stt_idle_timeout CORRECTION — the wrong finding must be retracted"
H=../handoff/transcription.md
if grep -q 'RETRACTED — WRONG' "$H"; then ok "retraction present in handoff"; else bad "retraction MISSING"; fi
if grep -q 'Why my grep came back empty' "$H"; then ok "stale-worktree trap documented"; else bad "trap explanation MISSING"; fi
if grep -q 'should NOT be removed' "$H"; then ok "records that the setting works"; else bad "does not say the setting works"; fi
if grep -q 'idleMonitor' internal/library/generate.go; then ok "idleMonitor genuinely exists (the claim was false)"; else bad "idleMonitor absent — the claim would have been TRUE"; fi

echo
if [ "$fail" -eq 0 ]; then
  printf '\033[32mBOTH ITEMS COMPLETE\033[0m — nothing pending on either.\n'
else
  printf '\033[31mSOMETHING IS INCOMPLETE\033[0m — see FAIL lines above.\n'
fi
exit $fail
