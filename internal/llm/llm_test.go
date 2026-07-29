package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractKeywords(t *testing.T) {
	tests := []struct {
		question string
		notEmpty bool
	}{
		{"Who is the monster in Frankenstein?", true},
		{"What happens to Elizabeth?", true},
		{"the", false}, // all stop words
		{"", false},
	}

	for _, tt := range tests {
		result := extractKeywords(tt.question)
		if tt.notEmpty && result == "" {
			t.Errorf("extractKeywords(%q) returned empty", tt.question)
		}
	}

	// Should pick the longest keyword
	result := extractKeywords("Who is the creature in Frankenstein?")
	if result != "frankenstein" {
		t.Errorf("expected 'frankenstein', got %q", result)
	}
}

func TestNewClient(t *testing.T) {
	// Anthropic defaults
	c := NewClient(ProviderAnthropic, "test-key", "", "")
	if c.Model() == "" {
		t.Error("model should have default")
	}
	if c.baseURL != "https://api.anthropic.com" {
		t.Errorf("base url = %q", c.baseURL)
	}

	// OpenAI defaults
	c = NewClient(ProviderOpenAI, "test-key", "", "")
	if c.Model() != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", c.Model())
	}

	// Ollama defaults
	c = NewClient(ProviderOllama, "", "", "")
	if c.baseURL != "http://localhost:11434" {
		t.Errorf("ollama base url = %q", c.baseURL)
	}

	// Custom model and URL
	c = NewClient(ProviderAnthropic, "key", "claude-opus-4-20250514", "https://custom.proxy.com")
	if c.Model() != "claude-opus-4-20250514" {
		t.Errorf("custom model = %q", c.Model())
	}
	if c.baseURL != "https://custom.proxy.com" {
		t.Errorf("custom url = %q", c.baseURL)
	}

	// Google (Gemini) defaults: OpenAI-compat base + a Gemini model.
	c = NewClient(ProviderGoogle, "key", "", "")
	if c.baseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Errorf("google base url = %q", c.baseURL)
	}
	if !strings.HasPrefix(c.Model(), "gemini-") {
		t.Errorf("google default model = %q, want a gemini-* model", c.Model())
	}
}

// TestGoogleChatCompletionsPath proves the Gemini path (its compat surface has
// no /v1 prefix) and that Complete drives the OpenAI-compatible request/response
// shape against a mock, so a Google credential genuinely serves LLM rather than
// being an offer that fails at use.
func TestGoogleChatCompletionsPath(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "gemini-1.5-flash" {
			t.Errorf("model in body = %v", req["model"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "gemini-1.5-flash",
			"choices": []map[string]any{{"message": map[string]string{"content": "hello from gemini"}}},
			"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 4},
		})
	}))
	defer srv.Close()

	c := NewClient(ProviderGoogle, "test-key", "", srv.URL+"/v1beta/openai")
	resp, err := c.Complete(CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1beta/openai/chat/completions" {
		t.Errorf("path = %q, want the compat /chat/completions (no /v1 prefix)", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want Bearer test-key", gotAuth)
	}
	if resp.Content != "hello from gemini" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}
