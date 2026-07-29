package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// The summary's cause field is what stops the Retry button lying: 187 min of
// PJ's library is fixed by re-running STT and 74 min is not, and the number of
// missing minutes cannot tell them apart.
func TestGapSummaryCause(t *testing.T) {
	gapsJSON := `[{"start_sec":600,"end_sec":1800,"duration_sec":1200,"source_file":"01.mp3"}]`

	newWork := func(t *testing.T, srv *Server, store *db.Store, title, path string,
		gaps string, scan *db.SourceScanRow) int64 {
		t.Helper()
		workID, _ := store.CreateWork(title, "A")
		store.UpsertBook(db.Book{WorkID: workID, Path: path, Filename: "01.mp3",
			Format: "mp3", MediaType: "audio", Origin: "narrator_recording"})
		bookID := bookIDByPath(t, store, path)
		if gaps != "" {
			store.SaveTranscriptionGaps(bookID, gaps)
		}
		if scan != nil {
			scan.BookID = bookID
			if err := store.SaveSourceScan(*scan); err != nil {
				t.Fatalf("save scan: %v", err)
			}
		}
		return workID
	}

	srv, store, _ := newTestServer(t)

	// Source verified readable, narration still missing -> the pipeline dropped
	// it, and re-running genuinely fixes it. (Bonfire / Feynman.)
	dropped := newWork(t, srv, store, "Dropped", "/library/a/01.mp3", gapsJSON,
		&db.SourceScanRow{DecodeErrors: 0})
	// Audio was never written -> retry is futile. (Oryx and Crake.)
	trunc := newWork(t, srv, store, "Truncated", "/library/b/01.mp3", gapsJSON,
		&db.SourceScanRow{DecodeErrors: 1, Truncated: true, ZeroAt: 5 << 20})
	// Corrupt frames, and NO gap span at all — the Life of Pi case, where words
	// vanish without ever crossing the gap threshold.
	damagedNoGap := newWork(t, srv, store, "DamagedNoGap", "/library/c/01.mp3", "",
		&db.SourceScanRow{DecodeErrors: 7})
	// Gaps but never scanned -> must not be reported as safe to retry.
	unknown := newWork(t, srv, store, "Unknown", "/library/d/01.mp3", gapsJSON, nil)
	// Clean and complete -> absent from the summary entirely.
	clean := newWork(t, srv, store, "Clean", "/library/e/01.mp3", "",
		&db.SourceScanRow{DecodeErrors: 0})

	req := httptest.NewRequest("GET", "/api/transcription-gaps/summary", nil)
	rec := httptest.NewRecorder()
	srv.handleTranscriptionGapsSummary(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out []struct {
		WorkID       int64   `json:"work_id"`
		Cause        string  `json:"cause"`
		SegmentCount int     `json:"segment_count"`
		TotalMissing float64 `json:"total_missing_sec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	byWork := map[int64]string{}
	present := map[int64]bool{}
	for _, e := range out {
		byWork[e.WorkID] = e.Cause
		present[e.WorkID] = true
	}

	for _, tc := range []struct {
		id   int64
		want string
		name string
	}{
		{dropped, "dropped_segment", "clean source + gap"},
		{trunc, "truncated_source", "truncated source"},
		{damagedNoGap, "damaged_source", "damaged source with no gap"},
		{unknown, "unknown", "gap but never scanned"},
	} {
		if got := byWork[tc.id]; got != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.name, got, tc.want)
		}
	}

	// A damaged source must surface even with zero gap spans — that is exactly
	// the case the gap threshold cannot see.
	if !present[damagedNoGap] {
		t.Error("damaged source with no gaps is absent from the summary — the silent-loss case is invisible")
	}
	for _, e := range out {
		if e.WorkID == damagedNoGap && e.SegmentCount != 0 {
			t.Errorf("damaged-no-gap entry reports %d segments, want 0", e.SegmentCount)
		}
	}
	// A verified-clean, gapless book must not appear at all.
	if present[clean] {
		t.Error("clean work appears in the gap summary")
	}
}
