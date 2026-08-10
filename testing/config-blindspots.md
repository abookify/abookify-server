# Configuration blind-spot register — standing check (est. 2026-08-10)

THE INSTRUMENT, one level up from selfdesc-audit.md: that one asks what
our DATA misdescribes; this one asks what our TESTING cannot reach.
A configuration that ships but that no verification walks is a pass
that proves nothing about it. The TTS-only finding (server-web's live
loop structurally could not see the ebook-karaoke path, because the
human-default steers every live multi-edition work down the transcript
path) cost two rounds of wrong predictions — found deliberately it
would have cost a table row. Re-run this register whenever a default
changes, a new work shape ships, or a fixture is added.

METHOD: map the live library into shapes (script in the git log of this
file), then ask of each shape: which fixture or live gate actually
walks it end-to-end?

## Register (live library: 73 works)
| Shape (live count) | Reached by | Status | Owner / artifact needed |
|---|---|---|---|
| human+transcript+epub (44) | 8195, 8198, live 85 gate | COVERED | — |
| audio+transcript, no epub (12) | 8196 (438 Days) | COVERED | — |
| TTS-only narration (5 + showcase + every GPU-less user) | 8199 (my suite) — server-web's live loop CANNOT reach it | FOUND 2026-08-10, dispatched | server-web: permanent TTS-only work in its loop |
| human+TTS two narrations (2, incl. 85) | live 85 gate, 8197/8198 | COVERED | — |
| TEXT-ONLY, no audio (5) | NOTHING — every journey starts with play | BLIND | suite (mine): epub-only fixture + reader-only journey (open, read, nav, Q&A; no player asserts) |
| AUDIO-ONLY, no text yet (5) | NOTHING — the pre-transcription state every new audiobook passes through | BLIND | suite (mine): audio-only fixture + journey (player works, Transcribe CTA renders, no reader crash) |
| m4b/m4a embedded markers (4) | unit tests only; no e2e walks marker-authority | BLIND | suite (mine): small m4b fixture; assert chapter list == embedded markers (authority > detection) |
| first-run EMPTY library | NOTHING — all five fixtures are populated; server-web just built one-tap sample onboarding against exactly this | BLIND | trivial: fixture-server with no import; journey = empty state + one-tap import (server-web's assert, my harness) |
| embedding/DTW paragraph-follow (0 live, but the ¶ pill SHIPS) | NOTHING — no work anywhere exercises paragraph-follow | BLIND (latent) | mine: deliberately-different-edition pair fixture (abridged vs full from showcase materials); server-web: ¶ render assert |
| >2 narrations | none live, not a shipping shape | not tracked | — |

## Notes
- 8199 work 2 (Time Machine, human+transcript+epub) sits in the fixture
  UNEXERCISED — the gate runs work 1 only. Free coverage if a journey
  ever needs a second same-instance work.
- The register names and assigns; it does not authorize building. Every
  artifact above waits for its opener.
