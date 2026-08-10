# Self-description audit — standing check (est. 2026-08-10)

THE CLASS: data that describes itself in a way a reasonable consumer
misreads. Three found by accident cost days (edition label split;
coverage.pairs empty on by-construction word sync; the two below).
Found deliberately they cost minutes. This is a RUNNING habit, not a
one-off list: run the sweep whenever an API payload changes, and once
per phase otherwise.

## The three questions (run them against any payload you touch)
1. ZERO vs ABSENT: does `omitempty` drop a value where 0 is MEANINGFUL?
   `grep -rn "float64.*omitempty\|int.*omitempty" internal/db/db.go internal/library/*.go`
   then ask of each hit: "is zero a real value here?"
2. EMPTY vs NONE-OF-THAT-KIND: does an empty list mean two different
   things? (coverage.pairs meant both "nothing aligned" and
   "word-synced by construction, no alignment needed".)
3. COUNT WITHOUT PROJECTION: does a number say what it counts?
   (5 files vs 6 chapters — every rendered count must name its
   projection; canon.active carries both for audio.)

## Ledger
| Found | Instance | Status |
|---|---|---|
| by accident | edition label split (job resume dropped labels) | REPAIRED + v2 key |
| by accident (mobile) | coverage.pairs [] on TTS by-construction works | FIXED (tts_construction pair) |
| deliberate | Chapter.start_sec omitempty — "starts at 0" ≡ "untimed" | PENDING web+mobile acks |
| deliberate | Book.start_sec omitempty — file 1 of every multi-file book has NO offset (docs call it ground truth) | PENDING same ack |
| deliberate | PlaybackPosition.position_secs / Bookmark.start_word omitempty zeros | same patch, low stakes |
| deliberate | embedding-only works → coverage.pairs [] | FIXED 2026-08-10 (unit=paragraph pairs) |
