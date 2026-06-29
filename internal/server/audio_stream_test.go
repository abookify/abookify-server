package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// #219: audio streaming maps the right Content-Type per container (opus→ogg,
// m4a/m4b→mp4, …) and honors Range requests (206 partial) so seekable offline
// playback works. Table-driven over the formats added/used this session.
func TestStreamBookContentTypeAndRange(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID, _ := store.CreateWork("Audio Work", "Author")

	const payload = "0123456789ABCDEFGHIJ" // 20 bytes, easy to slice

	cases := []struct {
		format string
		want   string
	}{
		{"opus", "audio/ogg"},
		{"m4a", "audio/mp4"},
		{"m4b", "audio/mp4"},
		{"mp3", "audio/mpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			path := filepath.Join(dir, "part."+tc.format)
			if err := os.WriteFile(path, []byte(payload), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			if err := store.UpsertBook(db.Book{
				WorkID: workID, Path: path, Filename: "part." + tc.format,
				Format: tc.format, MediaType: "audio", Title: "Part",
			}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			bookID := bookIDByPath(t, store, path)

			// Full GET → 200, correct content-type, full body, Range-capable.
			req := httptest.NewRequest("GET", "/api/books/x/stream", nil)
			req.SetPathValue("id", itoa(bookID))
			rec := httptest.NewRecorder()
			srv.handleStreamBook(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("full GET status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != tc.want {
				t.Errorf("Content-Type = %q, want %q", ct, tc.want)
			}
			if rec.Header().Get("Accept-Ranges") != "bytes" {
				t.Errorf("Accept-Ranges = %q, want bytes (seekable)", rec.Header().Get("Accept-Ranges"))
			}
			if rec.Body.Len() != len(payload) {
				t.Errorf("full body = %d bytes, want %d", rec.Body.Len(), len(payload))
			}

			// Range request → 206 partial with Content-Range.
			rreq := httptest.NewRequest("GET", "/api/books/x/stream", nil)
			rreq.SetPathValue("id", itoa(bookID))
			rreq.Header.Set("Range", "bytes=2-5")
			rrec := httptest.NewRecorder()
			srv.handleStreamBook(rrec, rreq)
			if rrec.Code != http.StatusPartialContent {
				t.Fatalf("range status = %d, want 206", rrec.Code)
			}
			if rrec.Header().Get("Content-Range") == "" {
				t.Errorf("missing Content-Range on a 206")
			}
			if got := rrec.Body.String(); got != "2345" {
				t.Errorf("range body = %q, want %q", got, "2345")
			}
			if ct := rrec.Header().Get("Content-Type"); ct != tc.want {
				t.Errorf("range Content-Type = %q, want %q (preserved through 206)", ct, tc.want)
			}
		})
	}
}

// #219: a missing-on-disk file is a 404, not a 500/panic.
func TestStreamBookMissingFile(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID, _ := store.CreateWork("Gone", "Author")
	path := filepath.Join(dir, "vanished.opus")
	store.UpsertBook(db.Book{WorkID: workID, Path: path, Filename: "vanished.opus",
		Format: "opus", MediaType: "audio", Title: "Gone"})
	bookID := bookIDByPath(t, store, path) // no file written

	req := httptest.NewRequest("GET", "/api/books/x/stream", nil)
	req.SetPathValue("id", itoa(bookID))
	rec := httptest.NewRecorder()
	srv.handleStreamBook(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
