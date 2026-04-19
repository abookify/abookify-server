package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Client talks to the Kokoro TTS service (OpenAI-compatible API).
type Client struct {
	baseURL    string
	httpClient *http.Client
	// Per-Synthesize call deadline. Covers both connect and body-read. Default
	// is 30 minutes — chosen because a single ~500-word chunk normally finishes
	// in under a minute on CPU Kokoro, but a backed-up queue or a cold-start
	// can push well past the previous 10-minute cap (observed on Gulag
	// Archipelago: chunk 0 of 140 hit the 10-min timeout before Kokoro finished
	// streaming the audio body). 30m is generous headroom without masking a
	// truly hung service.
	PerRequestTimeout time.Duration
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		// No global http.Client timeout — we set per-request deadlines via
		// context so slow responses on a single call don't poison the client.
		httpClient:        &http.Client{},
		PerRequestTimeout: 30 * time.Minute,
	}
}

// BaseURL returns the configured service URL (for diagnostics / sidecar metadata).
func (c *Client) BaseURL() string { return c.baseURL }

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

// Synthesize converts text to audio bytes using Kokoro. Uses the Client's
// PerRequestTimeout as a context deadline covering both request and body read.
// Logs a heartbeat every 60s while waiting so long-running chunks show up in
// logs (user-visible confirmation that we're not just hung).
func (c *Client) Synthesize(text string, voice string) ([]byte, error) {
	if voice == "" {
		voice = "af_heart"
	}
	timeout := c.PerRequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body := map[string]any{
		"model":           "kokoro",
		"input":           text,
		"voice":           voice,
		"response_format": "mp3",
		"speed":           1.0,
	}

	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/audio/speech", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("tts request build failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Heartbeat logger — quiet unless the call runs long.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				log.Printf("tts: still waiting on Kokoro after %s (%d words)",
					time.Since(start).Truncate(time.Second), len(text)/5)
			}
		}
	}()
	defer close(done)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts request failed after %s: %w", time.Since(start).Truncate(time.Second), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tts error (status %d): %s", resp.StatusCode, string(errBody))
	}

	return io.ReadAll(resp.Body)
}
