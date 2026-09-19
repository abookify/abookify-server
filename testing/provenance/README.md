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

THE GATE — BUILT 2026-09-19 (abookify-server dc1eb8f). It checks CLEARED, not declared.

- `publish_check.py abook <file.abook>...` or `--remote <showcase-v1 asset name>...`
  → GREEN only if manifest.publishing.public is true, every source is cleared
  (with cleared_by), the cover is the bundled EPUB's own image or carries its own
  clearance, no equal-split audio, ATTRIBUTION.txt present. Notes the Gutenberg id.
- `publish_check.py site <marketing/site>` → GREEN only if every file under
  showcase/covers, showcase/samples and shots is in PROVENANCE.json, cleared,
  and hash-identical to when it was cleared.
- The ONLY sanctioned publish paths run it first and refuse on any red:
  `scripts/release-upload.sh <tag> <files>` and the meta repo's `bin/site-deploy`.
- Producing a clearable file: declare + clear each source via
  `PUT /api/books/{id}/provenance` (`GET /api/works/{id}/provenance` shows a
  dry-run `publishable` verdict), then export with `?public=1` — a 422 names
  every missing or uncleared book; success writes the `publishing` block and a
  generated ATTRIBUTION.txt. Cover sources are recorded at write time in
  `covers/work-N.jpg.source.json`; OpenLibrary/upload art is never bundled publicly.

Rule learned from Owl Creek: it HAD declared provenance ("verify before public
redistribution") and was published anyway — the gate checks CLEARED, not
DECLARED.
