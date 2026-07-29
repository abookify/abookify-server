package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestProbeOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "gpt-4o"}, {"id": "whisper-1"}, {"id": "tts-1"}, {"id": "text-embedding-3-small"},
		}})
	}))
	defer srv.Close()

	// Valid key → capabilities derived from the models actually present.
	caps := probeOpenAI("good-key", srv.URL)
	sort.Strings(caps)
	if strings.Join(caps, ",") != "llm,stt,tts" {
		t.Fatalf("expected llm,stt,tts from the model list, got %v", caps)
	}

	// A models list with only chat models verifies llm only — NOT stt/tts (don't
	// assume a vendor serves everything).
	llmOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "gpt-4o-mini"}}})
	}))
	defer llmOnly.Close()
	if got := probeOpenAI("k", llmOnly.URL); strings.Join(got, ",") != "llm" {
		t.Fatalf("chat-only account should verify llm only, got %v", got)
	}

	// Bad key → nothing verified. Empty key → no call, nil.
	if got := probeOpenAI("bad-key", srv.URL); len(got) != 0 {
		t.Fatalf("bad key should verify nothing, got %v", got)
	}
	if got := probeOpenAI("", srv.URL); got != nil {
		t.Fatalf("empty key should return nil without a call, got %v", got)
	}
}
