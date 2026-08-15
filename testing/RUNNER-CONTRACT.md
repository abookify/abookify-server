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

## Known debt (tracked, not hidden — RULE 1 applied to the runner itself)

- `run-web.js` currently uses blind `page.waitForTimeout(...)` for several
  READINESS waits (chapter render, reader mount, karaoke DOM). These flake under
  load — observed 2026-08-14 when the live server was busy with the showcase
  queue and the reader mounted late. They are to be converted to `waitFor` on a
  real condition (`#reader-<id>` present; `.sync-word` count > 0; audio `src`
  set) incrementally, WITHOUT destabilizing the currently-green fleet mid-gate.
  Until converted, treat a `run-web.js` red under heavy concurrent load as
  "re-run once on a quiet server before believing it" — and say so in the report.
