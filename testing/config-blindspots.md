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

## Sweep 2 (2026-08-15, post-Oryx-removal, post-paragraph-migration, post-task-12)
Shape map moved: human+TTS grew to 3 works (P&P + WotW gained fresh Kokoro
editions; grows toward ~13 as the showcase queue drains) — two-narration
class covered by live-85 gate. Empty-library row: server-web is actively
building the first-run funnel (task 8) — owner engaged, row stays open
until a journey exists. m4b (4) and ¶-paragraph-follow: STILL BLIND (asset-gated /
artifact-gated, see RUNNER-PROCEDURE.md's coverage decision).
TEXT-ONLY and AUDIO-ONLY: **CLOSED 2026-08-15** — fixtures 8194
(Sleepy Hollow epub, reader_only journey: chapters render + nav, no
player) and 8193 (single mp3, pretranscribe_play journey: audio plays +
Transcribe CTA), both green first run; run-web.js shape gate reports
standard journeys n/a-by-shape instead of failing them.

NEW BLIND ROW, minted THIS WEEK by task 12:
| Shape | Reached by | Status | Owner / artifact |
|---|---|---|---|
| a book with DEGRADED production testimony | fixture 8192 (degraded_testimony journey) | **CLOSED 2026-08-15** — authentic testimony from the real integrity check judging a crafted inconsistent sidecar; building it also caught the writer's first-import ordering bug | UI pill assert joins when server-web's selector exists |

Also monitored, not blind: CAS sidecar-present vs -absent editions
(transitional by design — key-miss regenerates; P&P pre-deploy files
gained sidecars on resume, WotW born with them).

## Sweep 3 (2026-09-18, the showcase close — three found by checking things that already read green)
| Shape | Reached by | Status | Owner / artifact |
|---|---|---|---|
| human+TTS two narrations, through the TIMING AUDIT | nothing — the audit read the TTS edition's `tts_construction` coverage pair (in-text = 0, never populated) and skipped every dual-edition work as "weak chain"; all 11 showcase works had NO verdict while the audit reported "24 ok" | **CLOSED 2026-09-18** (47c0420): by-construction pairs ignored by the audit and given in-text = 1; sweep 24 → 33 verdicts. Lesson for the register: an instrument's no-verdict count is itself a blind-spot signal — 48 "skipped" hid a whole shipping class | mine — the store-backed audit test now covers a healthy + drifted bake; a dual-edition fixture row in that test would pin this specific skip |
| a TWO-narration `.abook` on the RECEIVING server (distributable sample shape) | only 8197 (an "honest red") — no green path exists: the importer colocates both narrations in one dir, canon sees two voices in one edition → incoherent, resume n/a. Found when the first showcase cut (11 two-narration files) was gated on a fresh server | BLIND by product: the shape cannot be green until the importer separates editions per (origin, voice). Showcase ships single-narration files meanwhile | server-web: importer per-edition subdirs; then 8197 flips green (report it when it does — its red is load-bearing today) |
| a newcomer importing BOTH the human and the AI-narrated file of the SAME title | nothing — every fixture imports different titles because a same-title second import 409s on identity dedupe; the showcase now ships exactly this pair for 11 titles | BLIND (product question: merge into one work as a second edition vs refuse) | server-web/product decision; then a fixture with human+AI Carol imported in sequence |

Also from this sweep, monitored not blind: LibriVox per-file intros put the
first ebook word 25–55 s after the spoken "Stave N" (Carol) — the timing
audit's 30 s tolerance calls that DRIFT; it is a constant per-file offset,
not a growing one. Structural false positive, stated on the report, left.

## Notes
- 8199 work 2 (Time Machine, human+transcript+epub) sits in the fixture
  UNEXERCISED — the gate runs work 1 only. Free coverage if a journey
  ever needs a second same-instance work.
- The register names and assigns; it does not authorize building. Every
  artifact above waits for its opener.
- **Third instrument in the family (2026-09-18):** the self-description
  audit asks what our DATA misdescribes; this register asks what our TESTING
  cannot reach; `bin/timing-audit` now asks WHEN AN INSTRUMENT DECLINES TO
  JUDGE — its summary groups every no-verdict work by the shape it shares
  (skip reason, numbers stripped) so silences are counted, not listed. A
  shape the audit cannot judge is a shape it cannot fail; the dual-edition
  skip that hid all 11 showcase works would have been its own row.
