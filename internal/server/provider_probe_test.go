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

func TestProbeAnthropic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "good" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "claude-3-5-sonnet"}}})
	}))
	defer srv.Close()
	if got := probeAnthropic("good", srv.URL); strings.Join(got, ",") != "llm" {
		t.Fatalf("anthropic → llm only, got %v", got)
	}
	if got := probeAnthropic("bad", srv.URL); len(got) != 0 {
		t.Fatalf("bad key → nothing, got %v", got)
	}
}

func TestProbeGoogle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "models/gemini-1.5-pro"}}})
	}))
	defer srv.Close()
	got := probeGoogle("good", srv.URL)
	sort.Strings(got)
	if strings.Join(got, ",") != "llm,voice" {
		t.Fatalf("gemini key → llm,voice, got %v", got)
	}
	// The caveat: a Gemini key must NEVER claim tts (Google Cloud TTS is separate).
	for _, c := range got {
		if c == "tts" {
			t.Fatal("gemini key must not claim tts — Google Cloud TTS is a separate product")
		}
	}
	if got := probeGoogle("bad", srv.URL); len(got) != 0 {
		t.Fatalf("bad key → nothing, got %v", got)
	}
}
