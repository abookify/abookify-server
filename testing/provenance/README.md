# Provenance audit scripts (2026-09-18) — the seed of the publish gate

These found the two OpenLibrary commercial covers and the uncleared Owl Creek
recording on the public showcase release (see handoff server-web pm191/pm192).

- `remote_abook_audit.py <asset-name>...` — inspects `.abook` zips on the
  `showcase-v1` GitHub release WITHOUT downloading audio: reads the zip central
  directory over HTTP Range, then fetches only manifest.json, ATTRIBUTION.txt,
  cover.jpg and book.db. Reports per file: audio entry sizes (the EQUAL-SPLIT
  tell for commercial rips), audio origins/voices/filenames, text books, the
  attribution's Source/Reader lines, and which site cover file (if any) matches
  the bundled cover byte-for-byte.
- `cover_audit.py <this-dir> <asset-name>...` — for each file, compares
  cover.jpg to every image inside the bundled `originals/*.epub` and reads the
  EPUB's OPF identifiers (gutenberg.org id). A cover that matches the Gutenberg
  EPUB's own image is proven public domain; one that matches nothing needs its
  source proven (the two failures were OpenLibrary backfills — reproduce
  `FetchCoverFromOpenLibrary`'s exact request and hash-compare).

THE GATE (designed, UNBUILT — do not treat these scripts as the gate):
1. export: manifest.json gains a `provenance` block, per bundled source
   {kind, source_url, license, cleared: bool, cleared_by, note} and for the
   cover {source: epub|audio|openlibrary:<olid>|upload, cleared}; a `--public`
   export REFUSES unless every source and the cover are cleared.
2. the cover backfill (fetch-missing → OpenLibrary) records `openlibrary:<olid>`
   as the cover's source at fetch time; such a cover is never bundled publicly.
3. `bin/publish-check`: runs these physical checks + the cleared flags on every
   artifact, at the TWO places publishing happens — the release upload and the
   site deploy — and refuses on any red.
Rule learned from Owl Creek: it HAD declared provenance ("verify before public
redistribution") and was published anyway — the gate checks CLEARED, not
DECLARED.
