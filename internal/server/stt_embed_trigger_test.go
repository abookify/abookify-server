package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
)

// fakeEmbedServer serves the OpenAI /v1/embeddings endpoint, returning one small
// indexed vector per input text so EmbedBook can persist them without a network.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{0.1, 0.2, 0.3}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "embed-test"})
	}))
	t.Cleanup(s.Close)
	return s
}

func embeddedChunkCount(t *testing.T, store *db.Store, bookID int64) int {
	t.Helper()
	chunks, err := store.ListChunks(bookID)
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	n := 0
	for _, c := range chunks {
		if len(c.Embedding) > 0 {
			n++
		}
	}
	return n
}

// #159 regression: an STT job completing auto-embeds the work when an LLM is
// configured, so Q&A works on a freshly transcribed/added book WITHOUT a manual
// embeddings refresh PJ should never have to think about. This locks the
// OnJobUpdate → EmbedWorkAsync trigger, which shipped but was untested — 159 sat
// "pending" all session while actually being done, and this is what lets code
// tell finished from unfinished.
func TestOnJobUpdate_STTCompleted_EmbedsWork(t *testing.T) {
	srv, store, _ := newTestServer(t)
	workID, bookID := seedTextChapters(t, store, 3)

	// LLM configured → point the RAG client at a fake embeddings endpoint.
	es := fakeEmbedServer(t)
	srv.rag.Store(llm.NewRAG(store, llm.NewClient(llm.ProviderOpenAI, "test-key", "gpt-test", es.URL)))

	// Simulate the Generator reporting the STT job done for this work.
	srv.OnJobUpdate(library.JobStatus{Type: "stt", Status: "completed", WorkID: workID})

	// EmbedWorkAsync runs in a goroutine — poll for the effect (up to ~4s).
	for i := 0; i < 40 && embeddedChunkCount(t, store, bookID) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if got := embeddedChunkCount(t, store, bookID); got == 0 {
		t.Fatalf("STT completion did not auto-embed the work (0 embedded chunks) — the #159 trigger is broken")
	}
}

// A completed job that ISN'T an STT job does not trigger the embed — the trigger
// is specific to transcription producing new text.
func TestOnJobUpdate_NonSTT_DoesNotEmbed(t *testing.T) {
	srv, store, _ := newTestServer(t)
	workID, bookID := seedTextChapters(t, store, 2)
	es := fakeEmbedServer(t)
	srv.rag.Store(llm.NewRAG(store, llm.NewClient(llm.ProviderOpenAI, "test-key", "gpt-test", es.URL)))

	srv.OnJobUpdate(library.JobStatus{Type: "tts", Status: "completed", WorkID: workID})
	time.Sleep(300 * time.Millisecond) // give any stray goroutine a chance
	if got := embeddedChunkCount(t, store, bookID); got != 0 {
		t.Fatalf("a non-STT job should not auto-embed, got %d embedded chunks", got)
	}
}

// Control: with no LLM configured, the trigger is a safe no-op — chunks stay
// un-embedded (Q&A degrades to keyword search), no crash.
func TestEmbedWorkAsync_NoLLM_NoOp(t *testing.T) {
	srv, store, _ := newTestServer(t)
	workID, bookID := seedTextChapters(t, store, 2)
	srv.rag.Store(nil) // no LLM

	if err := library.ChunkBook(store, bookID); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if total, _ := store.ChunkCount(bookID); total == 0 {
		t.Fatal("expected chunks to exist for the control")
	}
	srv.EmbedWorkAsync(999999) // unknown work → no panic
	srv.EmbedWorkAsync(workID) // no LLM → must be a no-op
	if got := embeddedChunkCount(t, store, bookID); got != 0 {
		t.Fatalf("no-LLM embed should be a no-op, got %d embedded", got)
	}
}
