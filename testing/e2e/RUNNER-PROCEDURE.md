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
2. RESTART the fleet — kill by PARSED PID only (a pattern pkill matches
   the runner itself; known self-kill, exit 144):
   for port in 8195 8196 8197 8198 8199; do
     pid=$(ps aux | grep "abookify-e2e" | grep -- "--port $port" | grep -v grep | awk '{print $2}')
     if [ -n "$pid" ]; then kill "$pid" || true; fi
   done
   for d in weak:8195 poc:8196 repaired:8197 messy:8198 canon:8199; do
     dir=/tmp/abookify-e2e-${d%%:*}; port=${d##*:}
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
| live :7654 work 85 | 11/11 | none (a mid-run connection reset usually means another lane deployed — report it as an event, run the board ONCE more only if the process list shows the server restarted, and say you did) |

## Coverage decision (2026-08-15, deliberate — omissions are decisions)
IN THE FLEET (to be added; build commands live in the register):
- TEXT-ONLY fixture (8194) + reader-only journey — 5 live works, every
  ebook-only user, cheapest possible fixture (one epub, no engines).
- AUDIO-ONLY fixture (8193) + pre-transcription journey (player works,
  Transcribe CTA renders, no reader crash) — the state EVERY new
  audiobook passes through.
- DEGRADED-TESTIMONY row on 8198 — produced AUTHENTICALLY by
  reimporting its broken transcript through checkSidecarIntegrity
  (never hand-written; the table's rule applies to fixtures too).
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
