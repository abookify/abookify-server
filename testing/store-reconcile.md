# Store reconciliation — rows ↔ files, both directions (board 18)

Third member of the instrument family: `selfdesc-audit.md` (data misdescribing
itself), `config-blindspots.md` (testing that cannot reach), and THIS (the two
stores disagreeing with each other). It exists because in one week of August
2026 both drift directions happened and nothing noticed either: a deleted
book's playback position survived and kept accepting writes (rows without
files), and three `/generated` edition dirs with zero book rows sat on disk
unreachable (files without rows). Each cost a round trip to find by accident.

**It is a reporter, not a cleaner.** It opens the database read-only, walks the
filesystem with `stat`/`readdir` only, and never deletes, renames or writes.
Deleting on inference is the fault class the August purge reconciliation was
about — a person reads the report and decides.

## Run

Where the server's paths resolve (the generated volume is root-only on the host,
so on tank that means inside the server container):

```bash
docker exec server-server-1 go run ./cmd/store-reconcile \
    -db /app/data/abookify.db -generated /generated            # text
docker exec server-server-1 go run ./cmd/store-reconcile \
    -db /app/data/abookify.db -generated /generated -json      # machine-readable
```

Desktop bundle: `-db ~/.abookify/abookify.db -generated ~/.abookify/generated`.
`-library a,b` overrides the `library_roots` table when the host maps paths
differently. `-fail-on high|med` turns it into a gate (exit 1) for a
once-per-phase check. The same sweep is available in-process as
`(*db.Store).Reconcile(opts)` for server-web to wrap in `GET /api/reconcile`
(their surface; the report struct JSON-marshals as is).

## What it reports

| direction | class | means |
|---|---|---|
| rows_without_files | `library_root_unreachable_or_partial` | >50 % of a root's indexed files missing → ONE finding (unmounted drive), never N deletions |
| rows_without_files | `book_file_missing` | a book row whose file is gone: lists, streams 404, positions against it keep saving |
| rows_without_files | `tts_sidecar_dangling_cas_key` | a chapter's `.mp3.cas` names a CAS object that is gone (plays via hardlink; re-synthesizes next generate) |
| rows_without_rows | `<table>.<col>_dangling` | a reference whose target row is gone — the tables the boot sweep (`CleanupOrphanedRows`) does NOT cover: positions/bookmarks/conditions by `book_id`, provenance, summaries, source_scans, text_trust, timing_results, Q&A, … |
| rows_without_rows | `<table>.book_id_unset` | `book_id = 0`: the row cannot name its file |
| rows_without_rows | `<table>_cross_work` | a position/bookmark whose book belongs to another work (merge/split leftover) |
| files_without_rows | `library_media_unindexed` | media under a root no row indexes: invisible in the library |
| files_without_rows | `library_abooks_unpacked_media` | media under `<root>/abooks/` (scanner-skipped by design) — invisible AND on disk |
| files_without_rows | `tts_edition_dir_unreferenced` | a generated edition dir with ZERO rows into it — the three-orphan-dirs class |
| files_without_rows | `tts_chapter_file_unreferenced` | a chapter file inside a live edition dir that no row serves |
| derived | `cas_object_unreferenced` · `waveform_cache_stale` · `tts_sidecar_without_chapter` · `tts_work_in_progress` | caches/objects that outlived their owner — disk, not correctness |
| INFO | `library_voice_previews` · `library_abook_archives` | expected, never indexed |

`internal/db/reconcile_test.go` plants every class once and asserts each count
(and that the tree is byte-identical afterwards) — the number moves, or the
test fails.

## Baseline reading — 2026-09-22 (tank, live library)

CLEAN (no HIGH/MED). 1030 indexed files under `/library`, 0 missing; 261 TTS
edition files under `/generated`, 0 missing; 217 CAS objects, all named by a
sidecar. What it did surface, all LOW/INFO:

- `source_scans.book_id_dangling` 11 and `text_trust.work_id_dangling` 1 —
  the boot sweep does not cover these two tables.
- `playback_positions.book_id_unset` 1 — work 102 (Sherlock Holmes) has a
  resume row with `book_id = 0`, saved 2026-08-15.
- `library_abooks_unpacked_media` 6 files / 67 MB — the Aug-7 colocated-dir
  import fixtures (Carol, Alice) under `/library/abooks/`, scanner-skipped.
- `waveform_cache_stale` 17 — cache entries for the book ids those fixtures
  had before they were deleted.
- `library_abook_archives` 31 / 9.8 GB — the offline export set (expected).

Cadence, same as the siblings: on demand, and once per phase before anything
is purged.
