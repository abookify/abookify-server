# Runner contract — the rules every journey runner obeys

Shared law for the browser/emulator journey runners (`testing/e2e/run-web.js`,
`testing/e2e/run-mobile.js`) and the fixture-fleet procedure that drives them
(`testing/e2e/RUNNER-PROCEDURE.md`). Established 2026-08-15, server-web, from
mobile's finding below. If a runner and this contract disagree, the runner is
wrong — fix the runner.

## Why this exists

Mobile's first fixture-fleet outing **buried a flake**: a transient infra
failure (the app had dropped to HOME) made a step unreachable, the runner
silently retried, and it reported only the green. The machinery built to stop
unearned greens produced one on day one. The fix was not "retry more carefully"
— it was to move retry OUT of the runner entirely.

## RULE 1 — A green must be green about something specific

Every run prints what it actually looked at (`run-web.js`'s "SCOPE OF THIS RUN"
block) and what it structurally **cannot** see ("CANNOT SEE (by construction)").
A journey that sampled one karaoke window says so; it does not report "the book
is fine". Silent coverage caps (top-N, one-window, no-sweep) are stated, never
implied.

## RULE 2 — Expected reds are load-bearing

Each board/fixture has an EXPECTED outcome, including named expected reds (e.g.
8198's `switch_source` + `surface_consistency`). Deviation in EITHER direction
is a finding: an expected red going **green** means the suite lost its ability
to fail and is reported as loudly as a green going red. A runner that can only
notice regressions is half-blind.

## RULE 3 — Report what was seen, verbatim, never a summary of a summary

Per-assert PASS/FAIL with detail; the failing screenshot is an artifact, not a
description of one. Exit code is mechanical (any fail → nonzero). "Deployed" is
never written as "verified"; "the job completed" is never written as "the output
is correct".

## RULE 4 — The runner NEVER retries. The harness absorbs known transients.

This is the one that bit mobile. Split the two jobs that a naive runner conflates:

- **The harness** absorbs a fixed, named set of KNOWN transients — for a browser
  journey: a page not yet painted, a resource still loading, a websocket not yet
  connected, a fixture port still draining. Each is a **deterministic bounded
  wait-loop whose every attempt is recorded in the report.** The set is decided
  in code, up front; nothing else is absorbed.
- **The runner** reports what it saw and decides nothing. It never re-attempts an
  action, never "tries once more", never swallows an error and continues. A
  runner that chooses what to re-attempt is choosing what to hide.

Everything the harness does not explicitly absorb is an HONEST RED, even if a
retry "would probably have worked". That temptation is the failure mode.

### The primitive: a recorded, bounded wait (adopt this, do not hand-roll)

```js
// Absorbs ONE named transient as a bounded loop; every attempt is an artifact.
// Returns the satisfied value; throws with the recorded attempts on timeout.
// It waits on a CONDITION becoming true — it never re-runs an action.
async function waitFor(label, cond, { timeoutMs = 15000, intervalMs = 250 } = {}) {
  const t0 = Date.now();
  let attempts = 0;
  while (Date.now() - t0 < timeoutMs) {
    attempts++;
    try { const v = await cond(); if (v) { record(label, 'ready', attempts); return v; } } catch {}
    await new Promise(r => setTimeout(r, intervalMs));
  }
  record(label, 'TIMEOUT', attempts);          // the wait is in the artifacts either way
  throw new Error(`waitFor(${label}) timed out after ${attempts} attempts`);
}
```

`record()` appends `{label, outcome, attempts}` to the run report. A green run
therefore shows its waits ("welcome-screen ready after 3 attempts"); a red shows
the wait that timed out. The number of attempts is never a knob the runner turns
— it is evidence the reader sees.

### Bounded SETTLE vs blind READINESS wait

A fixed `sleep(1500)` used as a **post-action settle** ("let the UI react, then
read the result") is acceptable — it is bounded and it is not standing in for a
readiness check. A fixed sleep used as a **readiness wait** ("it's probably
loaded by now") is a RULE-4 VIOLATION: it flakes under load and hides the wait.
Convert every readiness sleep to `waitFor`; a settle may stay a bounded sleep,
but prefer `waitFor` wherever a real condition exists to test.

## RULE 5 — A readiness condition must mean what the sleep stood for

Learned converting run-web.js (2026-09-18). Replacing `sleep(1800)` with "the
detail's menu button exists" fired in ~5 ms — 1.8 s EARLIER than the sleep — and
the drive that followed raced the still-in-flight hydrate: both seeks landed at
0:00 and the run went 9/11 while the OLD runner, kept as a CONTROL and run on the
same server in the same minute, was 11/11. Three corollaries, all bitten today:

- **The condition is the drive's OUTCOME, not any state that happens to be true.**
  "Audio is playing" was already true from the previous step; the right condition
  for "seek to 697 s" is *the book clock sits at ~697 s*. Name the target in the
  wait label (`seek-landed@697s`) so the artifact says what was waited for.
- **Two async drives on one player: land the first before issuing the second.** A
  faster second seek loses to a slower first one that resolves later.
- **Keep the previous runner as a control** until the converted one is green on
  the same board in the same session. A conversion that is faster AND red is the
  conversion's bug until the control says otherwise.
- **A precondition must be unambiguous.** Planting a resume position at exactly a
  file edge let the player's auto-advance re-render the reader between the poll
  and the assert. Plant ≥30 s from any edge, inside the DISPLAY edition.
- **A green that a recorded TIMEOUT contradicts is a finding.** Work 85 passed
  resume_flow while `resume-landed@~9372s` TIMED OUT: the plant had fallen into the
  other edition and the map-extent assert was too weak to fail (RULE 2). The
  assert now also requires landing within 90 s of the plant.

## Guard matching (the "notarized" class)

A guard whose match is looser than its intent fires on the name of the thing it
watches for: a CI tripwire for "Notarize" cancelled a healthy run because the
BUILD step's name contained "notarized next step". In run-web.js the same class
was `text=/AUDIOBOOK|EBOOK/i` (matches the library's own copy), `[class*=card]`,
and `/transcribe/i` over the whole body. Match ids and exact selectors
(`#work-detail[data-hydrated="<id>"]`, `#gen-text-<id>`, `.work-card`), never a
word that can appear in prose or in a label.

## Conversion debt — CLOSED 2026-09-18 (task 15)

Every readiness wait in `run-web.js` is a recorded, bounded `waitFor`; the two
remaining fixed sleeps are MEASUREMENT windows (6 s clock check, 10 s karaoke
window), which are meant to be fixed. In-page waits (shape boards, change_chapter)
are bounded polls whose attempt counts are returned and recorded. Drive-step
errors are recorded under their journey and fail it (`DRIVE ERRORS` block). The
last `networkidle` goto (resume_flow's reload) is gone. Proven on a quiet server:
8199 canon 11/11, 8198 messy 9/11 (its two named reds intact), live 7654 works
85 + 28 11/11 — with every wait's attempts in the report and zero drive errors.
The product gained one readiness signal for this: `#work-detail` sets
`data-hydrated=<workId>` when every async hydrate step has settled.

History of the debt (for the reasoning, not the status):

- `run-web.js` currently uses blind `page.waitForTimeout(...)` for several
  READINESS waits (chapter render, reader mount, karaoke DOM). These flake under
  load — observed 2026-08-14/15 when the live server was busy with the showcase
  queue and the reader mounted late. They are to be converted to `waitFor` on a
  real condition (`#reader-<id>` present; `.sync-word` count > 0; audio `src`
  set) incrementally, WITHOUT destabilizing the currently-green fleet mid-gate.
  Until converted, treat a `run-web.js` red under heavy concurrent load as
  "re-run once on a quiet server before believing it" — and say so in the report.

  CONVERSION DEBT LIST (do on a QUIET server, after the showcase queue drains —
  converting mid-multi-day-queue destabilises a thing you cannot cleanly observe):
  - `open_library` — DONE (2026-08-15, 5d46ef0): networkidle → recorded wait.
  - `karaoke_advances` — PENDING. Its blind post-play settles let the active-word
    read happen before the reader/sync finished mounting under load → active=null,
    a FALSE red (verified not-a-regression 2026-08-15: `resume_flow`'s karaoke
    check passed on the same work + the highlight was visually confirmed lit).
    Convert its settles to `waitFor('.sync-word.read count > 0')` before sampling.
  - remaining `waitForTimeout` readiness waits across the other journeys — audit
    each: a post-action SETTLE may stay; a READINESS wait becomes `waitFor`.
