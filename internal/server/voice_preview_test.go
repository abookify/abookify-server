package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestValidVoice(t *testing.T) {
	ok := []string{"af_heart", "am_michael", "bf_emma", "bm_george"}
	bad := []string{"", "af_heart/../../etc", "nope", "af heart", "../x", "zz_unknown"}
	for _, v := range ok {
		if !validVoice(v) {
			t.Errorf("validVoice(%q) = false, want true", v)
		}
	}
	for _, v := range bad {
		if validVoice(v) {
			t.Errorf("validVoice(%q) = true, want false", v)
		}
	}
}

// A cached preview is served as audio/mpeg without touching the TTS service;
// an unknown/unsafe voice is 404; a valid voice with no cache + no TTS is 503.
func TestHandleVoicePreview(t *testing.T) {
	srv, _, dir := newTestServer(t)
	srv.LibraryDir = dir

	req := func(voice string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/tts/voices/"+voice+"/preview.mp3", nil)
		r.SetPathValue("voice", voice)
		rec := httptest.NewRecorder()
		srv.handleVoicePreview(rec, r)
		return rec
	}

	// Pre-seed a cache file for af_heart → served straight from disk.
	if err := os.MkdirAll(srv.voicePreviewDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("ID3fake-mp3-bytes")
	if err := os.WriteFile(srv.voicePreviewPath("af_heart"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := req("af_heart")
	if rec.Code != http.StatusOK {
		t.Fatalf("cached voice = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
	if rec.Body.String() != string(want) {
		t.Errorf("body = %q, want the cached bytes", rec.Body.String())
	}

	// Unknown / path-traversal voice → 404 (never generates).
	if rec := req("zz_unknown"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown voice = %d, want 404", rec.Code)
	}

	// Known voice, no cache, no TTS wired → 503 (not a crash).
	if rec := req("am_michael"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("uncached voice w/o TTS = %d, want 503", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(srv.voicePreviewDir(), "am_michael."+voicePreviewVersion+".mp3")); err == nil {
		t.Error("no cache file should be written when TTS is unavailable")
	}
}
