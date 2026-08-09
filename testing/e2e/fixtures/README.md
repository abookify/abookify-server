# E2E fixture library (files too large for git — regenerate as below)

| fixture            | shape                                              | source |
|--------------------|----------------------------------------------------|--------|
| clean-carol.abook  | 1 Kokoro narration + 1 epub — pristine word map    | export work 85 `?books=111504,<kokoro ids>` |
| messy-carol.abook  | 2 narrations + 2 texts incl. a MANGLED transcript  | full export of work 85 (as of 2026-08-09 — keep this artifact; it is the known-broken calibration state) |
| timemachine.abook  | human narration + transcript + epub, different title | showcase "The Time Machine - H G Wells.abook" |

The messy fixture is load-bearing: the suite must FAIL correctly on its
broken transcript (A1) and PASS its healthy journeys. A suite that only sees
the pristine fixture has not been tested.
