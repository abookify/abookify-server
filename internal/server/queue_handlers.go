package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/library"
)

// GET /api/queue/status — current snapshot of incoming/processing/failed.
func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	if s.Ingest == nil {
		// Queue not started (e.g. failed init). Return empty status rather
		// than 500 so the UI degrades gracefully.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"incoming":[],"processing":[],"failed":[]}`))
		return
	}
	status := s.Ingest.Status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// DELETE /api/queue/failed/{name} — remove a failed entry.
// {name} must not contain path separators (defensive against traversal).
func (s *Server) handleQueueRemoveFailed(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		http.Error(w, "library not configured", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	target := filepath.Join(s.LibraryDir, "failed", name)
	if err := os.RemoveAll(target); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.Events.Broadcast(Event{Type: "queue_updated"})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/works/{id}/reprocess — rerun the full post-processing pipeline
// against a work's existing sidecar. Cheap (seconds) since this only redoes
// chapter detection, transcript split, paragraphs, RAG chunks — never the
// expensive Whisper transcription.
//
// Contract: clobbers user-edited chapter rows. Documented limitation in the
// post-processing design talk; future work could honor a "source: user"
// flag to preserve hand-edits.
func (s *Server) handleReprocessWork(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		http.Error(w, "library not configured", http.StatusInternalServerError)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	if err := library.ReimportWork(s.store, id, s.LibraryDir); err != nil {
		// Specific error mapping: missing-sidecar / no-audio is 4xx
		// (the work isn't in a state where reprocess is meaningful);
		// import failures are 5xx.
		msg := err.Error()
		if strings.Contains(msg, "no sidecar") || strings.Contains(msg, "not found") || strings.Contains(msg, "no audio") {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	// LLM fallback: ask the configured provider to label any
	// "Chapter N" rows the narrator didn't title. No-op when no LLM
	// is configured.
	if rag := s.RAG(); rag != nil && rag.Client() != nil {
		if err := library.LabelMissingChapterTitles(s.store, rag.Client(), id); err != nil {
			// Non-fatal — chapters keep their bare titles, reprocess
			// still succeeds.
			s.Events.Broadcast(Event{Type: "library_updated"})
		}
	}
	// Tell connected clients to refresh their library view so chapters
	// + sync get reloaded from the new DB rows.
	s.Events.Broadcast(Event{Type: "library_updated"})
	// Reprocess can re-chunk (new, unembedded chunks); embed them so Q&A stays
	// current without a restart (#159b — idempotent, no-op when no LLM).
	s.EmbedNewWorks()
	// Chapter content may have changed → drop cached summaries so they
	// regenerate from the new text on next request (#134).
	if work, _ := s.store.GetWork(id); work != nil {
		for _, tf := range work.TextFiles {
			s.invalidateSummaries(tf.ID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"work_id": id,
		"status":  "reprocessed",
	})
}

// handleReextractWork RE-EXTRACTS a work's ebook chapters from the source EPUB,
// then re-chunks + re-embeds. Every normal path (scan/rescan/watcher/reprocess)
// treats extraction as one-time and skips a book that already has chapters, so an
// improvement to the extractor (e.g. a new boilerplate strip) does nothing for
// books already in the DB. This is the deliberate re-derive: drop the old
// chapters + chunks, re-run ExtractEPUBChapters (picking up the current stripper),
// re-chunk, bump content_version (so the coherence watcher + mobile update-check
// see the change), and re-embed. Alignment is re-run separately via /align, which
// re-derives the coverage %. Goes through the server's single serialized DB
// connection, so it is safe alongside an external repair writing other works.
func (s *Server) handleReextractWork(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	work, err := s.store.GetWork(id)
	if err != nil || work == nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}
	reextracted, chapters := 0, 0
	for _, b := range work.TextFiles {
		if b.Format != "epub" {
			continue // only EPUB chapters come from an extractor we can re-run
		}
		chs, err := library.ExtractEPUBChapters(b.Path, b.ID)
		if err != nil {
			http.Error(w, "re-extract "+b.Filename+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.DeleteChaptersByBook(b.ID); err != nil {
			http.Error(w, "clear chapters: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.store.DeleteChunksByBook(b.ID)
		for _, ch := range chs {
			if err := s.store.InsertChapter(ch); err == nil {
				chapters++
			}
		}
		library.ChunkBook(s.store, b.ID)
		reextracted++
	}
	if reextracted == 0 {
		http.Error(w, "work has no EPUB text to re-extract", http.StatusBadRequest)
		return
	}
	s.store.BumpContentVersion(id)
	// Re-extraction changed the ebook's chapter offsets, so the whole derivation
	// chain downstream is stale: re-link chapters and re-align (which re-derives
	// the alignment payload the word map + coverage read from), then VERIFY the
	// result is complete. Doing link then align then verify here — instead of
	// leaving align "separate via /align" — is what stops an update from silently
	// leaving derived data half-finished.
	rep, rederiveErr := s.rederiveWork(id)
	if rederiveErr != nil {
		applog.Warnf("system", "reextract: re-derive work %d: %v", id, rederiveErr)
	}
	s.Events.Broadcast(Event{Type: "library_updated"})
	s.EmbedNewWorks()
	for _, tf := range work.TextFiles {
		s.invalidateSummaries(tf.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"work_id":    id,
		"status":     "reextracted",
		"epub_books": reextracted,
		"chapters":   chapters,
		"derivation": rep,
	})
}

// rederiveWork re-runs a work's post-import derivation chain — link chapters,
// re-align (re-derives the alignment payload the reader's word map + coverage
// read from) — then VERIFIES the result is complete. The verify is the point: a
// chain that half-finishes must not report success. STT sync_data is
// transcription output, unaffected by ebook/link changes, so it isn't re-run
// here (a changed ebook doesn't re-transcribe the audio); the verify still
// asserts sync completeness and flags any narration a prior run left partial.
func (s *Server) rederiveWork(id int64) (library.DerivationReport, error) {
	work, err := s.store.GetWork(id)
	if err != nil || work == nil {
		return library.DerivationReport{}, fmt.Errorf("work %d not found", id)
	}
	if err := library.LinkChapters(s.store, work); err != nil {
		return library.DerivationReport{}, err
	}
	if _, err := library.ComputeAnchorAlignment(s.store, id); err != nil {
		applog.Warnf("system", "rederive: align work %d: %v", id, err)
	} else {
		s.stampWork(id)
	}
	fresh, err := s.store.GetWork(id)
	if err != nil || fresh == nil {
		return library.DerivationReport{}, fmt.Errorf("reload work %d: %v", id, err)
	}
	rep, err := library.VerifyWorkDerivation(s.store, fresh)
	if err != nil {
		return rep, err
	}
	if !rep.OK {
		applog.Warnf("system", "rederive: work %d (%s) still incomplete after re-derive: %+v",
			id, rep.Title, rep.Issues)
	}
	return rep, nil
}

// POST /api/works/{id}/rederive — re-run the derivation chain and verify. The
// repair vehicle (fixes a work whose chapter links collapsed) and the manual
// trigger for the same chain the update path runs.
func (s *Server) handleRederive(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	rep, err := s.rederiveWork(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Events.Broadcast(Event{Type: "library_updated"})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"work_id": id, "derivation": rep})
}

// GET /api/works/{id}/derivation — read-only completeness verdict for one work.
func (s *Server) handleWorkDerivation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	work, err := s.store.GetWork(id)
	if err != nil || work == nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}
	rep, err := library.VerifyWorkDerivation(s.store, work)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}

// GET /api/derivation-check — library-wide sweep; returns only the works whose
// derived karaoke data is incomplete. Empty = the whole library asserts whole.
func (s *Server) handleDerivationCheck(w http.ResponseWriter, r *http.Request) {
	bad, err := library.StartupDerivationSweep(s.store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"count": len(bad), "incomplete": bad})
}

// GET /api/works/{id}/transcription-gaps — list audio spans where
// Whisper produced no output. Empty list = analyzed cleanly; missing
// = pre-gap-detection sidecar import, reprocess the work to populate.
func (s *Server) handleTranscriptionGaps(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid work id", http.StatusBadRequest)
		return
	}
	work, err := s.store.GetWork(id)
	if err != nil || work == nil {
		http.Error(w, "work not found", http.StatusNotFound)
		return
	}
	type bookGaps struct {
		BookID   int64           `json:"book_id"`
		Filename string          `json:"filename"`
		Analyzed bool            `json:"analyzed"`
		Gaps     json.RawMessage `json:"gaps"`
	}
	var out []bookGaps
	for _, b := range work.AudioFiles {
		raw, err := s.store.GetTranscriptionGaps(b.ID)
		if err != nil {
			continue
		}
		entry := bookGaps{BookID: b.ID, Filename: b.Filename}
		if raw == "" {
			entry.Analyzed = false
			entry.Gaps = json.RawMessage("[]")
		} else {
			entry.Analyzed = true
			entry.Gaps = json.RawMessage(raw)
		}
		out = append(out, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// GET /api/transcription-gaps/summary — one-shot rollup for the library
// page: returns one entry per work that has at least one detected gap,
// with the total missing time and a list of source files. The UI uses
// this to decorate work cards with a warning badge without N+1
// requests against the per-work endpoint.
func (s *Server) handleTranscriptionGapsSummary(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	// cause is what the UI keys its four states on. Untranscribed time alone
	// cannot express the difference between "re-running STT fixes this" and
	// "the audio was never delivered", and offering one Retry across both is a
	// lie for half of them — 187 min of PJ's library is recoverable that way and
	// 74 min is not.
	//
	// Derived from the PERSISTED SOURCE SCAN rather than from gap geometry: if a
	// file decodes cleanly and narration is still absent, the pipeline dropped
	// it, and that inference needs no guessing about bucket boundaries.
	type summaryEntry struct {
		WorkID        int64    `json:"work_id"`
		TotalMissingS float64  `json:"total_missing_sec"`
		SegmentCount  int      `json:"segment_count"`
		SourceFiles   []string `json:"source_files,omitempty"`
		Cause         string   `json:"cause"`
	}
	type gapShape struct {
		StartSec    float64 `json:"start_sec"`
		EndSec      float64 `json:"end_sec"`
		DurationSec float64 `json:"duration_sec"`
		SourceFile  string  `json:"source_file"`
	}
	var out []summaryEntry
	for _, wk := range works {
		var entry summaryEntry
		seen := map[string]bool{}
		ids := make([]int64, 0, len(wk.AudioFiles))
		for _, b := range wk.AudioFiles {
			ids = append(ids, b.ID)
			raw, err := s.store.GetTranscriptionGaps(b.ID)
			if err != nil || raw == "" || raw == "[]" {
				continue
			}
			var gaps []gapShape
			if err := json.Unmarshal([]byte(raw), &gaps); err != nil {
				continue
			}
			for _, g := range gaps {
				entry.TotalMissingS += g.DurationSec
				entry.SegmentCount++
				if g.SourceFile != "" && !seen[g.SourceFile] {
					entry.SourceFiles = append(entry.SourceFiles, g.SourceFile)
					seen[g.SourceFile] = true
				}
			}
		}

		// Source integrity outranks gap geometry: a truncated or damaged file
		// explains the absence AND determines the remedy.
		scans, _ := s.store.GetSourceScans(ids)
		var truncated, damaged, scannedClean int
		for _, id := range ids {
			sc, ok := scans[id]
			if !ok {
				continue
			}
			switch {
			case sc.Truncated:
				truncated++
			case sc.DecodeErrors > 0:
				damaged++
			default:
				scannedClean++
			}
		}

		switch {
		case truncated > 0:
			entry.Cause = "truncated_source"
		case damaged > 0:
			entry.Cause = "damaged_source"
		case entry.SegmentCount > 0 && scannedClean == len(ids) && len(ids) > 0:
			// Every source verified readable, yet narration is missing — the
			// only remaining explanation is that transcription dropped it, and
			// re-running genuinely fixes it.
			entry.Cause = "dropped_segment"
		case entry.SegmentCount > 0:
			// Gaps with no (or partial) scan. Never guess that a retry is safe.
			entry.Cause = "unknown"
		}

		// A damaged source is reported even with no gap span: that is exactly the
		// case where words vanish without ever crossing the gap threshold —
		// Life of Pi lost ~1,854 words that way and looked clean.
		if entry.SegmentCount > 0 || entry.Cause == "truncated_source" || entry.Cause == "damaged_source" {
			entry.WorkID = wk.ID
			out = append(out, entry)
		}
	}
	if out == nil {
		out = []summaryEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// POST /api/books/{id}/embed — backfill chunk embeddings for one book.
// Idempotent (skips chunks that already have embeddings). Returns counts.
func (s *Server) handleEmbedBook(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil {
		http.Error(w, "RAG not configured (no LLM provider set)", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid book id", http.StatusBadRequest)
		return
	}
	embedded, err := rag.EmbedBook(id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"book_id":  id,
		"embedded": embedded,
	})
}

// POST /api/works/{id}/scan-sources — decode every audio file of a work and
// persist whether it is readable, which is what gives the gap indicator its
// cause. Synchronous: the scan runs at roughly 760x realtime, so even a
// 27-hour book finishes in about half a minute, and the caller wants a result
// to render rather than a job to poll.
//
// A clean result is recorded too. "We looked and it was fine" is the fact that
// lets a book be called complete; without it the UI can only say it has never
// checked, which is the honest reading of an unscanned book.
func (s *Server) handleScanSources(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if s.LibraryDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "library path not configured",
		})
		return
	}
	scanned, damaged, err := library.ScanWorkSources(s.store, s.LibraryDir, id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id": id,
		"scanned": scanned,
		"damaged": damaged,
	})
}

// GET /api/works/{id}/text-trust — does this work's transcript match its audio?
//
// Always 200 with a state, including "unchecked" for a work nobody has examined.
// That distinction is the point: a silently unchecked book otherwise looks
// identical to one that passed, which is how 122,759 invented words stayed
// invisible.
func (s *Server) handleTextTrust(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	row, _ := s.store.GetTextTrust(id)
	writeJSON(w, http.StatusOK, library.BuildTextTrust(id, row))
}

// POST /api/works/{id}/text-trust/check — examine the work now and persist the
// verdict. Synchronous; parsing one sidecar costs ~140 ms.
func (s *Server) handleTextTrustCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if s.LibraryDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "library path not configured"})
		return
	}
	t, err := library.ComputeTextTrust(s.store, s.LibraryDir, id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if t == nil {
		writeJSON(w, http.StatusOK, library.BuildTextTrust(id, nil))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// GET /api/text-trust/summary — one row per work that has been checked, for the
// library listing. Works absent from the array have never been checked; the UI
// must render that as unknown rather than clean.
func (s *Server) handleTextTrustSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListTextTrust()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	out := make([]library.TextTrust, 0, len(rows))
	for id := range rows {
		row := rows[id]
		out = append(out, library.BuildTextTrust(id, &row))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SuspectPercent > out[j].SuspectPercent })
	writeJSON(w, http.StatusOK, out)
}
