# E2E fixture library (files too large for git — regenerate as below)

| fixture            | shape                                              | source |
|--------------------|----------------------------------------------------|--------|
| clean-carol.abook  | 1 Kokoro narration + 1 epub — pristine word map    | export work 85 `?books=111504,<kokoro ids>` |
| messy-carol.abook  | 2 narrations + 2 texts incl. a MANGLED transcript  | full export of work 85 (as of 2026-08-09 — keep this artifact; it is the known-broken calibration state) |
| timemachine.abook  | human narration + transcript + epub, different title | showcase "The Time Machine - H G Wells.abook" |

The messy fixture is load-bearing: the suite must FAIL correctly on its
broken transcript (A1) and PASS its healthy journeys. A suite that only sees
the pristine fixture has not been tested.

## 2026-09-18 — provenance notes (read before trusting a copy)
- `clean-carol.abook` = the showcase export `~/abookify-showcase/A Christmas Carol -
  Charles Dickens (AI-narrated).abook` (same export lineage, same book ids). A copy
  rescued from an old session scratchpad had a book.db whose sha256 did NOT match
  its own manifest (edited after export) and the importer correctly refused it
  (`book.db checksum: mismatch`) — kept as `*.TAMPERED-*.bad`, never use it.
- `messy-carol.abook` (the load-bearing broken artifact) matches its manifest and
  imports; it is the ONLY surviving copy — keep it here (gitignored) and back it up.
- `timemachine.abook` = the showcase Time Machine export.
- Check any copy before booting: `unzip -p X.abook book.db | sha256sum` must equal
  the `checksums["book.db"]` in `unzip -p X.abook manifest.json`.
