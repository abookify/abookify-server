package llm

import (
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
}
