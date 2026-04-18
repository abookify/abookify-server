package tts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to the Kokoro TTS service (OpenAI-compatible API).
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

// Health checks if the TTS service is available.
func (c *Client) Health() error {
	resp, err := c.httpClient.Get(c.baseURL + "/v1/models")
	if err != nil {
		return fmt.Errorf("tts service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("tts service unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// Synthesize converts text to audio bytes using Kokoro.
func (c *Client) Synthesize(text string, voice string) ([]byte, error) {
	if voice == "" {
		voice = "af_heart"
	}

	body := map[string]any{
		"model":           "kokoro",
		"input":           text,
		"voice":           voice,
		"response_format": "mp3",
		"speed":           1.0,
	}

	jsonBody, _ := json.Marshal(body)
	resp, err := c.httpClient.Post(c.baseURL+"/v1/audio/speech", "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("tts request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tts error (status %d): %s", resp.StatusCode, string(errBody))
	}

	return io.ReadAll(resp.Body)
}
