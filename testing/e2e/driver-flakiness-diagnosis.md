# Mobile e2e driver flakiness — diagnosis (2026-08-15)

Meta asked for a real diagnosis of the "marginal emulator" flakiness before
writing more journeys on top of it — because a journey written against a ~50%
flaky driver produces a suite nobody trusts. This is that diagnosis.

## Verdict

**Not "this host can't drive an emulator."** The flakes have TWO layers, both
now named, one fixable in our code and one an environmental amplifier:

1. **Proximate cause (our code, FIXABLE — fixed):** the driver's input
   primitives are *fire-and-forget with no verify-then-retry.* `tap(x,y)` and
   `typeText(s)` just fire `adb input`; `tapText` taps coordinates from a
   possibly-stale dump. When the UI hasn't settled, the input is silently
   dropped. Every driving flake this session is this pattern at a specific step.

2. **Root amplifier (the host, NOT our lane):** the host is **memory-starved.**
   Measured at idle 2026-08-15: RAM 44.9/62 GB used, **swap 30.7 GB used with
   17 MB free (fully exhausted)**, **load average ~10**. On a swap-exhausted,
   high-load host the software-GPU (`swiftshader_indirect`) emulator's render +
   input latency balloons *and varies wildly*, which is what turns "fire-and-
   forget input" into an intermittent drop instead of a reliable one.

So the flake rate is `driver has no retry` × `host makes latency high + random`.
Fixing either reduces it; fixing the driver makes the suite tolerate the host.

## The specific flaky steps and their fixes (all the same shape)

| Step | Failure under load | Fix (harness owns the retry) | Commit |
|---|---|---|---|
| AVD memory | 2 GB AVD → LMK OOM-kills the app → "app at HOME" | launch `-memory 6144` | (recipe) |
| `connect()` | library RENDER overran the 25 s wait → false `connected=false` | 25 s → 45 s (`waitFor` polls, returns early on a fast render) | `fc2d95f` |
| `openReaderPaused()` | app transiently backgrounded → "Open reader" absent → INFRA | retry pause→dump→tap 3×, re-front the app's task | `adff8e6` |
| search filter | `tap(field)+typeText` outran the field focus → type dropped → card "not found" | retry tap+type 3×, verify the card surfaced, clear partial first | `a104b6c` |
| SystemUI/launcher ANR | slow GPU render at boot → ANR dialogs block driving | settle-wait (poll + dismiss) before driving | (procedure) |

Pattern: **wait for the expected state / retry the input — never fire-and-forget
and never let a runner re-run to hide it** (RUNNER-CONTRACT.md rule 4).

## What is NOT the cause

- **Not the app.** The reader advanced `[20/20]` (widx 344→372) on good sync
  this same session; play/connect/library all pass once the input lands.
- **Not a crash/LMK at 6 GB.** With `-memory 6144` the app stays foreground
  (empty `logcat -b crash`); the LMK kills that remain are background gms/vending, normal.
- **Not the AVD per se.** It boots and runs; it was only under-provisioned.

## Strategic implication (changes the runner strategy)

Full green-suite validation — and especially *adding* a longer, more stateful
journey like `download_offline_play` — should run when the host has **memory
headroom** (swap not exhausted, load not ~10) or on **less-loaded / dedicated
test hardware / CI**. Driving a 6 GB emulator on top of a swap-at-17 MB host also
**risks the shared machine** (PJ's desktop + the other lanes), so opportunistic
runs during a busy multi-day queue are the wrong time. Server-web reached the
same restraint independently on the web runner (#15): convert blind waits on a
QUIET server, because doing it mid-queue destabilises a thing you can't cleanly
observe.

**Bottom line for the journey work:** the driver is now hardened at every step
that flaked this session (verify-and-retry in the harness). Write
`download_offline_play` against the hardened driver, but validate its flake rate
on a quiet host before trusting it — and don't confuse a host-starvation red with
a product red (the exit-3 INFRA path already separates the harness-didn't-drive
case; keep new steps on that discipline).
