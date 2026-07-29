package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Client talks to the faster-whisper STT HTTP service.
type Client struct {
	baseURL    string
	httpClient *http.Client

	// DisableVAD turns off the service's voice-activity filter for this client.
	//
	// The VAD silently DISCARDS audio it does not judge to be speech, and it is
	// wrong often enough to matter: The Da Vinci Code's closing credits and a
	// 32-second stretch of The Handmaid's Tale were dropped entirely, producing
	// spans that looked like transcription failures. Both transcribe correctly
	// with the filter off. Default (false) preserves existing behaviour, since
	// the VAD earns its place on most material.
	DisableVAD bool

	// DisableConditioning stops the decoder conditioning on its own previous
	// output. Conditioning improves continuity but can trap the model in a
	// repetition loop, and on some stretches it suppresses transcription
	// altogether. Default (false) preserves existing behaviour.
	DisableConditioning bool
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			// Transcription of long files can take a while. faster-whisper
			// large-v3 on CPU runs at ~0.4x realtime, so a single 10-min
			// chunk is ~25 min — right at the edge of a 30-min timeout.
			// Bumping to 90 min gives headroom for queued chunks plus
			// slow runs on cold caches; GPU runs finish in seconds.
			Timeout: 90 * time.Minute,
		},
	}
}

type TranscribeResult struct {
	Language            string    `json:"language"`
	LanguageProbability float64   `json:"language_probability"`
	Duration            float64   `json:"duration"`
	Text                string    `json:"text"`
	Segments            []Segment `json:"segments"`
}

type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	Words []Word  `json:"words,omitempty"`
}

type Word struct {
	Word        string  `json:"word"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Probability float64 `json:"probability"`
}

// Health checks if the STT service is available.
func (c *Client) Health() error {
	resp, err := c.httpClient.Get(c.baseURL + "/health")
	if err != nil {
		return fmt.Errorf("stt service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("stt service unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// Info reports the STT service's active model + compute device, read from its
// /health endpoint. This is the ground truth of what transcription is actually
// running on (cpu vs cuda) — used to surface "GPU available / running on X" via
// the server API. compute_type + gpu_available are absent on older services.
type Info struct {
	Model        string `json:"model"`
	Device       string `json:"device"`
	ComputeType  string `json:"compute_type"`
	GPUAvailable bool   `json:"gpu_available"`
}

func (c *Client) Info() (*Info, error) {
	// Short deadline — this backs a cheap info endpoint, not a transcription,
	// so it must never inherit the client's 90-minute timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stt service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("stt service unhealthy: status %d", resp.StatusCode)
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode stt info: %w", err)
	}
	// Older services don't send gpu_available; derive it from the device.
	if info.Device == "cuda" {
		info.GPUAvailable = true
	}
	return &info, nil
}

// Unload tells the whisper service to free its model from RAM/VRAM (idle
// unloading). The next TranscribeFile transparently reloads it. Returns whether
// a model was actually freed (false if it was already unloaded). A short
// deadline — this is a control call, never inherits the 90-minute transcribe
// timeout.
func (c *Client) Unload() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/unload", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("stt unload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("stt unload: status %d", resp.StatusCode)
	}
	// The two speech backends report the freed-a-model signal under DIFFERENT
	// keys: the hermetic engine (engine/stt_server.py) returns {"unloaded":bool};
	// the Docker whisper (services/whisper/server.py) returns {"was_loaded":bool}.
	// Parse both so "freed" is accurate regardless of which backend is running —
	// without this, an unload against Docker whisper always reported freed=false
	// (the model WAS freed server-side; the Go side just never knew, so the
	// "freed ~3 GB" log never fired). Both services should converge on one shape;
	// until they do, accept either.
	var out struct {
		Unloaded  bool `json:"unloaded"`   // hermetic engine
		WasLoaded bool `json:"was_loaded"` // Docker whisper
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode unload result: %w", err)
	}
	return out.Unloaded || out.WasLoaded, nil
}

// TranscribeFile sends an audio file for transcription and returns the result.
func (c *Client) TranscribeFile(audioPath string) (*TranscribeResult, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, fmt.Errorf("open audio: %w", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}

	// Request word-level timestamps for alignment
	if err := w.WriteField("word_timestamps", "true"); err != nil {
		return nil, err
	}
	// Only send the toggles when overriding, so a service predating them is
	// unaffected and the default request is byte-identical to before.
	if c.DisableVAD {
		if err := w.WriteField("vad_filter", "false"); err != nil {
			return nil, err
		}
	}
	if c.DisableConditioning {
		if err := w.WriteField("condition_on_previous_text", "false"); err != nil {
			return nil, err
		}
	}

	w.Close()

	req, err := http.NewRequest("POST", c.baseURL+"/transcribe", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stt request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("stt error (status %d): %s", resp.StatusCode, string(errBody))
	}

	var result TranscribeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}
