package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Seek B contract (docs/seek-index-design.md): ?t= anchors an MP3 stream at an
// accurate frame. The branches that don't need ffmpeg (parse/format guards) are
// tested unconditionally; the frame-resolving branches need ffprobe on PATH and
// a real MP3, so they're skipped where ffprobe is absent (e.g. the CI go image).
func TestStreamSeekContract(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID, _ := store.CreateWork("Seek Work", "Author")

	// A tiny non-MP3 file: ?t= must be IGNORED (m4b/opus seek natively).
	m4bPath := filepath.Join(dir, "a.m4b")
	os.WriteFile(m4bPath, []byte("0123456789"), 0644)
	store.UpsertBook(db.Book{WorkID: workID, Path: m4bPath, Filename: "a.m4b", Format: "m4b", MediaType: "audio", Title: "M4B"})

	// A placeholder MP3 for the parse-guard tests (malformed t is rejected before
	// ffprobe ever runs, so its bytes don't need to be a real MP3).
	mp3Path := filepath.Join(dir, "a.mp3")
	os.WriteFile(mp3Path, []byte("not a real mp3 but enough for the 400 path"), 0644)
	store.UpsertBook(db.Book{WorkID: workID, Path: mp3Path, Filename: "a.mp3", Format: "mp3", MediaType: "audio", Title: "MP3"})

	books, _ := store.ListBooks()
	var m4bID, mp3ID int64
	for _, b := range books {
		switch b.Format {
		case "m4b":
			m4bID = b.ID
		case "mp3":
			mp3ID = b.ID
		}
	}

	get := func(id int64, q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/books/"+itoa(id)+"/stream"+q, nil)
		req.SetPathValue("id", itoa(id))
		srv.handleStreamBook(rec, req)
		return rec
	}

	// Malformed t -> 400 (guarded before any ffprobe call).
	if rec := get(mp3ID, "?t=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed t: got %d, want 400", rec.Code)
	}
	// ?t= on a non-MP3 is ignored -> the whole file streams (200/206, not 400).
	if rec := get(m4bID, "?t=abc"); rec.Code == http.StatusBadRequest {
		t.Errorf("t on non-mp3 should be ignored, got 400")
	}

	// The frame-resolving branches need ffprobe + a real MP3.
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH; skipping frame-resolution asserts")
	}
	realMP3 := os.Getenv("SEEK_TEST_MP3")
	if realMP3 == "" {
		t.Skip("set SEEK_TEST_MP3 to a real VBR mp3 to assert frame resolution")
	}
	store.UpsertBook(db.Book{WorkID: workID, Path: realMP3, Filename: "real.mp3", Format: "mp3", MediaType: "audio", Title: "Real"})
	var realID int64
	allb, _ := store.ListBooks()
	for _, b := range allb {
		if b.Filename == "real.mp3" {
			realID = b.ID
		}
	}
	if rec := get(realID, "?t=60"); rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		t.Errorf("valid seek: got %d", rec.Code)
	} else if rec.Header().Get("X-Audio-Start-Sec") == "" {
		t.Errorf("valid seek missing X-Audio-Start-Sec header")
	}
	if rec := get(realID, "?t=999999"); rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("past-end seek: got %d, want 416", rec.Code)
	}
}
