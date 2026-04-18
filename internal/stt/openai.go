// OpenAI Whisper STT provider. Uses the OpenAI Audio Transcriptions API (BYOK).
// https://platform.openai.com/docs/api-reference/audio/createTranscription
package stt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenAIClient implements Provider for OpenAI's Whisper API.
type OpenAIClient struct {
	apiKey     string
	model      string // "whisper-1"
	baseURL    string
	httpClient *http.Client
}

func NewOpenAIClient(apiKey string) *OpenAIClient {
	return &OpenAIClient{
		apiKey:  apiKey,
		model:   "whisper-1",
		baseURL: "https://api.openai.com",
		httpClient: &http.Client{
			Timeout: 30 * time.Minute,
		},
	}
}

func (c *OpenAIClient) Name() string { return "openai-whisper" }

func (c *OpenAIClient) Health() error {
	if c.apiKey == "" {
		return fmt.Errorf("OpenAI API key not configured")
	}
	return nil
}

// TranscribeFile calls OpenAI's /v1/audio/transcriptions endpoint.
// Returns word-level timestamps via the "verbose_json" response format.
func (c *OpenAIClient) TranscribeFile(audioPath string) (*TranscribeResult, error) {
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
	w.WriteField("model", c.model)
	w.WriteField("response_format", "verbose_json")
	w.WriteField("timestamp_granularities[]", "word")
	w.Close()

	req, _ := http.NewRequest("POST", c.baseURL+"/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai whisper error %d: %s", resp.StatusCode, string(errBody))
	}

	// OpenAI returns a different JSON structure from our local whisper:
	// { text, task, language, duration, words: [{word, start, end}], segments: [...] }
	var raw struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Words    []struct {
			Word  string  `json:"word"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"words"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Convert to our standard TranscribeResult format.
	result := &TranscribeResult{
		Language: raw.Language,
		Duration: raw.Duration,
		Text:     raw.Text,
	}
	// Build segments with embedded words.
	if len(raw.Segments) > 0 {
		for _, seg := range raw.Segments {
			s := Segment{Start: seg.Start, End: seg.End, Text: strings.TrimSpace(seg.Text)}
			// Attach words that fall within this segment's time range.
			for _, w := range raw.Words {
				if w.Start >= seg.Start && w.End <= seg.End+0.1 {
					s.Words = append(s.Words, Word{
						Word:  w.Word,
						Start: w.Start,
						End:   w.End,
					})
				}
			}
			result.Segments = append(result.Segments, s)
		}
	} else if len(raw.Words) > 0 {
		// No segments — create one covering the whole file.
		seg := Segment{Start: 0, End: raw.Duration, Text: raw.Text}
		for _, w := range raw.Words {
			seg.Words = append(seg.Words, Word{
				Word:  w.Word,
				Start: w.Start,
				End:   w.End,
			})
		}
		result.Segments = []Segment{seg}
	}
	return result, nil
}
