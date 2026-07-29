package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Provider represents an LLM provider.
type Provider string

const (
	ProviderAnthropic  Provider = "anthropic"
	ProviderOpenAI     Provider = "openai"
	ProviderOllama     Provider = "ollama"
	ProviderOpenRouter Provider = "openrouter"
	// ProviderGoogle is Gemini via its OpenAI-compatible endpoint (Bearer auth,
	// chat-completions request/response shape) — so it reuses completeOpenAI. It
	// is an LLM path only; a Google credential's other verified kinds (voice) are
	// separate features, and Google Cloud TTS is a different product entirely.
	ProviderGoogle Provider = "google"
)

// Client is a multi-provider LLM client.
type Client struct {
	provider   Provider
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type CompletionRequest struct {
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	System      string    `json:"system,omitempty"`
}

type CompletionResponse struct {
	Content string `json:"content"`
	Model   string `json:"model"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewClient(provider Provider, apiKey, model, baseURL string) *Client {
	if model == "" {
		switch provider {
		case ProviderAnthropic:
			model = "claude-sonnet-4-20250514"
		case ProviderOpenAI:
			model = "gpt-4o"
		case ProviderOllama:
			model = "llama3.2"
		case ProviderOpenRouter:
			// Solid default — cheap and capable.
			model = "openai/gpt-4o-mini"
		case ProviderGoogle:
			model = "gemini-1.5-flash"
		}
	}
	if baseURL == "" {
		switch provider {
		case ProviderAnthropic:
			baseURL = "https://api.anthropic.com"
		case ProviderOpenAI:
			baseURL = "https://api.openai.com"
		case ProviderOllama:
			baseURL = "http://localhost:11434"
		case ProviderOpenRouter:
			// OpenRouter's chat-completions endpoint sits at
			// /api/v1/chat/completions — same path shape as OpenAI's
			// /v1/chat/completions appended to this baseURL.
			baseURL = "https://openrouter.ai/api"
		case ProviderGoogle:
			// Gemini's OpenAI-compatible surface. The base already carries
			// /v1beta/openai, so the chat path is just /chat/completions
			// (see chatCompletionsURL).
			baseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
		}
	}

	return &Client{
		provider:   provider,
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Model() string {
	return c.model
}

func (c *Client) Complete(req CompletionRequest) (*CompletionResponse, error) {
	switch c.provider {
	case ProviderAnthropic:
		return c.completeAnthropic(req)
	case ProviderOpenAI, ProviderOpenRouter, ProviderGoogle:
		// OpenRouter and Gemini both implement the OpenAI chat-completions API
		// (Bearer auth, identical request/response shape); only the base URL and
		// chat path differ, both set in NewClient / chatCompletionsURL.
		return c.completeOpenAI(req)
	case ProviderOllama:
		return c.completeOllama(req)
	default:
		return nil, fmt.Errorf("unknown provider: %s", c.provider)
	}
}

func (c *Client) completeAnthropic(req CompletionRequest) (*CompletionResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	body := map[string]any{
		"model":      c.model,
		"max_tokens": maxTokens,
		"messages":   req.Messages,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	jsonBody, _ := json.Marshal(body)
	httpReq, _ := http.NewRequest("POST", c.baseURL+"/v1/messages", bytes.NewReader(jsonBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("anthropic error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	text := ""
	for _, c := range result.Content {
		text += c.Text
	}

	return &CompletionResponse{
		Content: text,
		Model:   result.Model,
		Usage:   result.Usage,
	}, nil
}

// chatCompletionsURL is the OpenAI-compatible chat endpoint for this client.
// Gemini's compat surface lives under /v1beta/openai (already in baseURL) so its
// path is /chat/completions; OpenAI and OpenRouter use /v1/chat/completions.
func (c *Client) chatCompletionsURL() string {
	if c.provider == ProviderGoogle {
		return c.baseURL + "/chat/completions"
	}
	return c.baseURL + "/v1/chat/completions"
}

func (c *Client) completeOpenAI(req CompletionRequest) (*CompletionResponse, error) {
	messages := req.Messages
	if req.System != "" {
		messages = append([]Message{{Role: "system", Content: req.System}}, messages...)
	}

	body := map[string]any{
		"model":    c.model,
		"messages": messages,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	jsonBody, _ := json.Marshal(body)
	httpReq, _ := http.NewRequest("POST", c.chatCompletionsURL(), bytes.NewReader(jsonBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	// OpenRouter recommends an attribution header so app-level rate-limit
	// rules and dashboards can identify traffic. No-op for OpenAI proper.
	if c.provider == ProviderOpenRouter {
		httpReq.Header.Set("HTTP-Referer", "https://abookify.local")
		httpReq.Header.Set("X-Title", "abookify")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	text := ""
	if len(result.Choices) > 0 {
		text = result.Choices[0].Message.Content
	}

	return &CompletionResponse{
		Content: text,
		Model:   result.Model,
		Usage: struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}{
			InputTokens:  result.Usage.PromptTokens,
			OutputTokens: result.Usage.CompletionTokens,
		},
	}, nil
}

func (c *Client) completeOllama(req CompletionRequest) (*CompletionResponse, error) {
	messages := req.Messages
	if req.System != "" {
		messages = append([]Message{{Role: "system", Content: req.System}}, messages...)
	}

	body := map[string]any{
		"model":    c.model,
		"messages": messages,
		"stream":   false,
	}

	jsonBody, _ := json.Marshal(body)
	httpReq, _ := http.NewRequest("POST", c.baseURL+"/api/chat", bytes.NewReader(jsonBody))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &CompletionResponse{
		Content: result.Message.Content,
		Model:   result.Model,
	}, nil
}
