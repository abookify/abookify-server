// OpenAI TTS provider. Uses the OpenAI Audio Speech API (BYOK).
// https://platform.openai.com/docs/api-reference/audio/createSpeech
package tts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIClient implements Provider for OpenAI's TTS API.
type OpenAIClient struct {
	apiKey     string
	model      string // "tts-1" or "tts-1-hd"
	baseURL    string
	httpClient *http.Client
}

func NewOpenAIClient(apiKey string) *OpenAIClient {
	return &OpenAIClient{
		apiKey:  apiKey,
		model:   "tts-1",
		baseURL: "https://api.openai.com",
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

func (c *OpenAIClient) Name() string { return "openai-tts" }

func (c *OpenAIClient) Health() error {
	if c.apiKey == "" {
		return fmt.Errorf("OpenAI API key not configured")
	}
	return nil // No health endpoint; key validity checked on first use.
}

// Synthesize calls OpenAI's /v1/audio/speech endpoint.
// Voice names: alloy, echo, fable, onyx, nova, shimmer.
func (c *OpenAIClient) Synthesize(text string, voice string) ([]byte, error) {
	if voice == "" {
		voice = "nova"
	}
	body := map[string]any{
		"model":           c.model,
		"input":           text,
		"voice":           voice,
		"response_format": "mp3",
	}
	jsonBody, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", c.baseURL+"/v1/audio/speech", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai tts request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai tts error %d: %s", resp.StatusCode, string(errBody))
	}

	return io.ReadAll(resp.Body)
}
