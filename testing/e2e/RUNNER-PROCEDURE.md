# Fixture-fleet rebuild-and-regate — runner procedure
(est. 2026-08-15, written once green became steady state; contract:
testing/RUNNER-CONTRACT.md, esp. RULE 4 — the runner NEVER retries;
every wait below is a deterministic bounded loop whose attempts are
part of the report.)

## What this certifies
The whole board after any change to server code or the web UI: five
hermetic fixture servers + the live server, each with an EXPECTED
outcome. Deviation in EITHER direction is reportable — an expected red
going green is as much a finding as a green going red.

## Steps (execute in order; report every command's output verbatim)
1. REBUILD the fixture binary from current source:
   cd engineering/server
   docker run --rm -v "$(pwd)":/app -w /app -e GOFLAGS=-buildvcs=false \
     -e CGO_ENABLED=0 golang:1.24-bookworm go build -o bin/abookify-e2e ./cmd/abookify
   Report: `ls -la bin/abookify-e2e` (mtime must be NOW; a stale mtime
   means the build silently failed — STOP and report, do not proceed).
   FIXTURE DATA LIVES UNDER $HOME/.abookify-e2e/<board>, NOT /tmp: a /tmp wipe
   (2026-09-18) took every fixture with it. Rebuild a board from its .abook
   inputs with fixture-server.sh (imports FAIL LOUDLY):
     F=testing/e2e/fixtures; R=$HOME/.abookify-e2e
     E2E_PORT=8199 E2E_DIR=$R/canon E2E_CLEAN_ABOOK=$F/clean-carol.abook \
       E2E_SECOND_ABOOK=$F/timemachine.abook setsid bash testing/e2e/fixture-server.sh
     E2E_PORT=8198 E2E_DIR=$R/messy E2E_CLEAN_ABOOK=$F/messy-carol.abook \
       E2E_SECOND_ABOOK=$F/timemachine.abook setsid bash testing/e2e/fixture-server.sh
   (messy Carol goes in the CLEAN slot so it is work 1 — importing it NEXT TO
   clean Carol 409s on identity dedupe.) The other boards' inputs (weak 8195,
   poc 8196, repaired 8197, textonly 8194, audioonly 8193, degraded 8192) were
   /tmp-only and have no committed recipe — rebuild from the register before
   trusting their rows below. The board that exists is what was run; say so.
2. RESTART the fleet — kill by PARSED PID only (a pattern pkill matches
   the runner itself; known self-kill, exit 144):
   for port in 8195 8196 8197 8198 8199; do
     pid=$(ps aux | grep "abookify-e2e" | grep -- "--port $port" | grep -v grep | awk '{print $2}')
     if [ -n "$pid" ]; then kill "$pid" || true; fi
   done
   for d in weak:8195 poc:8196 repaired:8197 messy:8198 canon:8199; do
     dir=$HOME/.abookify-e2e/${d%%:*}; port=${d##*:}
     nohup ./bin/abookify-e2e --data-dir "$dir" --port "$port" >/tmp/e2e-$port.log 2>&1 &
   done
3. HEALTH WAIT — bounded loop, attempts recorded, NEVER an ad-hoc retry:
   for port in 8195 8196 8197 8198 8199; do
     n=0
     until curl -sf -m 2 http://localhost:$port/api/health >/dev/null; do
       n=$((n+1)); [ $n -gt 45 ] && { echo "$port FAILED-TO-START after $n attempts"; break; }
       sleep 2
     done
     echo "$port up after $n wait-attempts"
   done
   A port that never comes up is a RED for this run — report it; do not
   restart it again yourself.
4. RUN THE BOARD (each once; live server needs the auth cookie):
   cd testing/e2e
   node run-web.js --base http://localhost:<port> --work 1        # fixtures
   node run-web.js --base http://localhost:7654 --work 85 --cookie <token>
   Token: sqlite3 ../../data/abookify.db \
     "select token from auth_sessions order by created_at desc limit 1"

## Expected outcomes (deviation EITHER WAY = finding)
| Board | Expect | Expected reds (verbatim journey names) |
|---|---|---|
| 8195 weak-chain | 11/11 | none |
| 8196 PoC 438 Days | 11/11 | none |
| 8197 repaired-carol | 10/11 | surface_consistency (honest: colocated-dir two-narration .abook shape) |
| 8198 messy | 9/11 | switch_source + surface_consistency (RED BY DESIGN — broken artifact; these reds are load-bearing: if either goes GREEN, the suite lost its ability to fail — report loudly) |
| 8199 clean carol | 11/11 | none |
| 8194 text-only | 3/3 (open_library, open_book, reader_only) | none — SHAPE board |
| 8193 audio-only | 3/3 (open_library, open_book, pretranscribe_play) | none — SHAPE board |
| 8192 degraded-testimony | 3/3 (open_library, open_book, degraded_testimony) | none — DATA-CONTRACT board (fixture: /tmp/abookify-e2e-degraded; crafted inconsistent sidecar judged by the real integrity check) |
| live :7654 work 85 | 11/11 | none (a mid-run connection reset usually means another lane deployed — report it as an event, run the board ONCE more only if the process list shows the server restarted, and say you did) |

## Coverage decision (2026-08-15, deliberate — omissions are decisions)
IN THE FLEET (to be added; build commands live in the register):
- TEXT-ONLY fixture (8194) + reader-only journey — BUILT 2026-08-15,
  green. Fixture: /tmp/abookify-e2e-textonly (Sleepy Hollow epub only).
- AUDIO-ONLY fixture (8193) + pre-transcription journey — BUILT
  2026-08-15, green. Fixture: /tmp/abookify-e2e-audioonly (one mp3).
  Fleet restart loop covers both: add textonly:8194 audioonly:8193 to
  the restart list in step 2.
- DEGRADED-TESTIMONY board: BUILT 2026-08-15 as its OWN fixture (8192)
  rather than on 8198 — mutating 8198 risked healing its load-bearing
  by-design reds. Input crafted (internally inconsistent sidecar);
  testimony authored ONLY by the real integrity check judging it.
  Add degraded:8192 to the fleet restart list.
LEFT OUT, with reasons:
- M4B embedded markers: needs a real marker-bearing sample (asset
  acquisition); the parsing path is unit-tested and stable, and doesn't
  churn with UI work. A one-off manual spot-check against a live m4b
  work covers it better than a per-run journey. On the register.
- ¶ PARAGRAPH-FOLLOW: requires a genuinely-different-edition pair
  (abridged vs full — real content + an alignment run); zero live works
  use the mode today. Gating every run on an artifact that does not yet
  exist would make the fleet slower AND dishonest. Stays on the
  register as its own opener.
The dividing line: the fleet covers what a USER hits weekly; the
register holds what needs a deliberate artifact first.
