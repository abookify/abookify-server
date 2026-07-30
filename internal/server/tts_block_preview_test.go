package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

type fakeTTS struct{ lastText, lastVoice string }

func (f *fakeTTS) Name() string  { return "fake" }
func (f *fakeTTS) Health() error { return nil }
func (f *fakeTTS) Synthesize(text, voice string) ([]byte, error) {
	f.lastText, f.lastVoice = text, voice
	return []byte("ID3fake-audio"), nil
}

func getPreview(srv *Server, bookID, idx, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/books/"+bookID+"/chapters/"+idx+"/tts-preview"+query, nil)
	r.SetPathValue("bookId", bookID)
	r.SetPathValue("idx", idx)
	rec := httptest.NewRecorder()
	srv.handleChapterTTSPreview(rec, r)
	return rec
}

// Invalid path values are 400 before anything else runs.
func TestChapterTTSPreview_BadInput(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if rec := getPreview(srv, "notanumber", "0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad bookId = %d, want 400", rec.Code)
	}
	if rec := getPreview(srv, "1", "x", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad idx = %d, want 400", rec.Code)
	}
}

// No TTS wired → 503, not a crash.
func TestChapterTTSPreview_NoTTS(t *testing.T) {
	srv, store, _ := newTestServer(t)
	seedPreviewChapter(t, store)
	if rec := getPreview(srv, "1", "0", "?voice=af_heart"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no TTS = %d, want 503", rec.Code)
	}
}

// Happy path: synthesizes a REAL block of the chapter (through PreprocessForTTS +
// SplitTextForTTS), returns audio, and reports the exact text + block counts.
func TestChapterTTSPreview_SynthesizesRealBlock(t *testing.T) {
	srv, store, dir := newTestServer(t)
	seedPreviewChapter(t, store)
	ft := &fakeTTS{}
	srv.Generator = library.NewGenerator(store, ft, nil, dir, nil)

	rec := getPreview(srv, "1", "0", "?voice=bm_george")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
	if rec.Body.String() != "ID3fake-audio" {
		t.Errorf("body = %q, want the synthesized audio", rec.Body.String())
	}
	// The chosen voice is threaded through to the real synth call.
	if ft.lastVoice != "bm_george" {
		t.Errorf("synth voice = %q, want bm_george", ft.lastVoice)
	}
	// What was synthesized is real chapter prose, not a canned sentence.
	if !strings.Contains(ft.lastText, "whispering woods") {
		t.Errorf("synth text %q should be the chapter's own prose", ft.lastText)
	}
	// The exact previewed text is reported (base64) so the wizard can show it.
	dec, err := base64.StdEncoding.DecodeString(rec.Header().Get("X-Preview-Text"))
	if err != nil || string(dec) != ft.lastText {
		t.Errorf("X-Preview-Text = %q (decoded %q), want == synth text %q", rec.Header().Get("X-Preview-Text"), string(dec), ft.lastText)
	}
	if rec.Header().Get("X-Preview-Total-Blocks") == "" {
		t.Error("X-Preview-Total-Blocks not set")
	}
}

// A missing chapter is 404 (with TTS wired, so we get past the 503 gate).
func TestChapterTTSPreview_ChapterNotFound(t *testing.T) {
	srv, store, dir := newTestServer(t)
	seedPreviewChapter(t, store)
	srv.Generator = library.NewGenerator(store, &fakeTTS{}, nil, dir, nil)
	if rec := getPreview(srv, "1", "99", "?voice=af_heart"); rec.Code != http.StatusNotFound {
		t.Errorf("missing chapter = %d, want 404", rec.Code)
	}
}

// Asking for a block past the end clamps to the last block (still 200).
func TestChapterTTSPreview_BlockClamped(t *testing.T) {
	srv, store, dir := newTestServer(t)
	seedPreviewChapter(t, store)
	srv.Generator = library.NewGenerator(store, &fakeTTS{}, nil, dir, nil)
	rec := getPreview(srv, "1", "0", "?voice=af_heart&block=9999&words=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("clamped block = %d, want 200", rec.Code)
	}
}

func seedPreviewChapter(t *testing.T, store *db.Store) {
	t.Helper()
	content := "The house stood at the edge of the whispering woods, where the tallest trees " +
		"leaned close over a narrow path. Every morning a thin mist rose from the hollow, and " +
		"the villagers said the woods remembered every traveller who had ever passed through them. " +
		"On this particular day, a young reader set out along that path with a lantern and a single book."
	if err := store.InsertChapter(db.Chapter{
		BookID: 1, Index: 0, Title: "Chapter One", Content: content, WordCount: len(strings.Fields(content)),
	}); err != nil {
		t.Fatalf("insert chapter: %v", err)
	}
}
