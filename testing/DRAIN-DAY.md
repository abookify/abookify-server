# Drain-day runbook (written 2026-08-15, BEFORE the event)
The most coupled scheduled moment on PJ's live library. Sequencing has
bitten twice (a deploy landing mid-queue; an edition removal landing
mid-session) — this is the third event, written down first.

## The honest coupling answer
Mostly independent and redoable. TWO soft orderings and ONE trap:
- SOFT 1: queue must be DRAINED before the deploy restart (a restart
  mid-job auto-resumes but re-synthesizes the in-flight chapter; CAS
  makes that cheap, not free).
- SOFT 2: the export must FOLLOW Carol's landing (the .abook should
  carry the new edition) and SHOULD follow the ingest (exports carve
  the DB; sidecars/conditions are files+rows the ingest completes).
- TRAP: mobile MUST be told when Carol lands — its book ids change a
  second time. Last time this landed mid-session it froze their reader
  (task 16). Tell them BEFORE, not after.

## Sequence, with per-step checks and rollbacks
0. VERIFY DRAINED: jobs table shows 11 tts-* completed, 0 failed/queued.
   Each edition dir has its chapter files + registered books. WotW
   onward: .cas sidecars + complete testimony. P&P: NO sidecars, NO
   testimony (pre-deploy completion — expected, fixed by step 2).
   Check: coherence sweep (expect 0 incoherent). Live-85 gate 11/11.
   If a job FAILED: it re-queues independently; nothing else blocks.
1. DEPLOY HEAD (restart server; brings in: canon text conditions,
   testimony ordering fix, DeleteBook cascade, coverage/canon additions
   since 95ffa39). No schema changes beyond tables the running binary
   already created. Check: /api/health, boot sweep completes in logs,
   coherence 0, live-85 gate 11/11. ROLLBACK: git checkout <prev> +
   restart — no data migration to unwind.
2. CAS INGEST (bin/tts-cas-ingest -db ./data/abookify.db -generated
   <host generated path>): stamps P&P's 28 pre-deploy files from
   current text. Writes NO testimony (ingest is not production; P&P
   reads unknown until next producer touch — honest). Idempotent;
   rerunnable; independent of step 1 in code but run after for one
   moving part at a time. Check: chapter-000.mp3.cas exists for P&P;
   count stamped == log line.
3. CAROL VERIFICATION (landed with the queue, nothing to run): canon 85
   — human edition STILL the active default; kokoro edition
   condition=complete, key tts:bm_fable; live-85 gate 11/11; spot-check
   whisper on chapter-001 opening (epub text, PAUSED cadence silences
   present at -70dB). THEN TELL MOBILE the ids changed (the trap).
4. SCRATCH-WORK GENERATE (server-web's default fix, unit→live proof):
   pick a scratch work with epub+transcript, POST generate-audio with
   NO text_book_id, verify the job id names the EPUB. Delete the
   scratch edition after (DeleteBook cascade now covers positions).
5. ELEVEN-BOOK EXPORT: per work, POST export with audio; verify each
   .abook's book.db (11 audio rows for multi-file works? no — per-work
   counts), attribution present, coverage pair method tts_construction.
   Each export independent + rerunnable. Files land in exports/; copy
   to ~/abookify-showcase/ replacing the 4 old Kokoro abooks (KEEP the
   old ones until the new pass verification — approved/candidate rule).
6. CLOSING SWEEP: full fleet (8 boards) + live gate + coherence; update
   SHOWCASE-MANIFEST.md; board tasks 7/14 to review with the record.

## If step N fails after 1..N-1 landed
Every step is individually redoable; nothing in 1..5 destroys prior
state (the only deletion anywhere is the scratch edition in step 4,
whose cascade is the thing step 4 partly proves). The genuinely
unredoable thing on drain day is nothing — that is the design, and if
an unredoable step appears in the doing, STOP and write it here first.
