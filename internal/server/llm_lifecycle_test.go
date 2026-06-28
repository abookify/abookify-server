package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSettings(t *testing.T, srv *Server, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSaveSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save settings %s → %d (%s)", body, rec.Code, rec.Body.String())
	}
}

// #160: saving llm_* settings must rebuild the RAG client in place (no restart)
// — enabling a provider makes RAG live, clearing it disables RAG. (Ollama needs
// no key, so it exercises the path without a real cloud credential. The empty
// test library means the #159 enable-backfill goroutine is a no-op.)
func TestSaveSettingsReloadsLLM(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.ReloadLLM() // boot state: no provider configured
	if srv.RAG() != nil {
		t.Fatal("RAG should be nil before any provider is configured")
	}

	// Enable via the save handler → RAG goes live without a restart (#160).
	postSettings(t, srv, `{"llm_provider":"ollama","llm_base_url":"http://127.0.0.1:1"}`)
	if srv.RAG() == nil {
		t.Error("RAG should be non-nil after saving llm_provider=ollama (reload-on-save)")
	}

	// A non-llm save must NOT touch the LLM client.
	postSettings(t, srv, `{"tts_voice":"am_adam"}`)
	if srv.RAG() == nil {
		t.Error("RAG should survive a non-llm settings save")
	}

	// Clearing the provider disables RAG.
	postSettings(t, srv, `{"llm_provider":""}`)
	if srv.RAG() != nil {
		t.Error("RAG should be nil after clearing llm_provider")
	}
}

// #160: a masked secret echoed back unchanged must NOT clobber the stored key,
// and the reload must still pick up the real key.
func TestSaveSettingsKeepsMaskedSecret(t *testing.T) {
	srv, store, _ := newTestServer(t)
	if err := store.SetSetting("llm_api_key", "sk-real-secret-value-1234"); err != nil {
		t.Fatal(err)
	}
	// Echo back the mask (as the UI does when the field is left untouched).
	masked := maskSecret("sk-real-secret-value-1234")
	postSettings(t, srv, `{"llm_provider":"openai","llm_api_key":"`+masked+`"}`)
	got, _ := store.GetSetting("llm_api_key")
	if got != "sk-real-secret-value-1234" {
		t.Errorf("masked echo clobbered the key: got %q", got)
	}
}

// #159b: EmbedNewWorks (the import/scan/enable backfill trigger) must be a
// no-op when no LLM is configured — it must not start a backfill goroutine.
func TestEmbedNewWorksNoLLM(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.ReloadLLM() // no provider → RAG nil
	srv.EmbedNewWorks()
	srv.embedAllMu.Lock()
	running := srv.embedAllRunning
	srv.embedAllMu.Unlock()
	if running {
		t.Error("EmbedNewWorks must not start a backfill when no LLM is configured")
	}
}

// #134: summary endpoints are key-gated — 503 when no LLM is configured.
func TestSummaryEndpointsGated(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.ReloadLLM() // no provider → RAG nil

	for _, path := range []string{"/api/books/1/chapters/0/summary", "/api/books/1/recap?up_to=0"} {
		req := httptest.NewRequest("GET", path, nil)
		req.SetPathValue("bookId", "1")
		req.SetPathValue("idx", "0")
		rec := httptest.NewRecorder()
		if strings.Contains(path, "recap") {
			srv.handleBookRecap(rec, req)
		} else {
			srv.handleChapterSummary(rec, req)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without LLM → %d, want 503", path, rec.Code)
		}
	}
}
