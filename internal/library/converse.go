// Voice conversation mode: speech-in → Q&A → speech-out.
//
// MVP flow (HTTP push-to-talk):
//   1. Client records user question, POSTs audio to /api/works/{id}/converse
//   2. Whisper transcribes the question to text
//   3. AskWithCitations retrieves + answers via LLM
//   4. Kokoro synthesizes the answer text
//   5. Response: {question, answer, citations, audio_base64}
//
// Full duplex streaming (WebSocket) is a follow-on — this covers the
// "cooking-and-asking" use case as a single round-trip.
package library

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

// ConverseResponse is the full round-trip result of one voice exchange.
type ConverseResponse struct {
	Question    string         `json:"question"`
	Answer      string         `json:"answer"`
	Citations   []llm.Citation `json:"citations"`
	Model       string         `json:"model"`
	AudioBase64 string         `json:"audio_base64"` // mp3, empty if TTS failed or unavailable
	AudioMIME   string         `json:"audio_mime"`
}

// Converse runs one round of voice conversation about a work.
//
// questionAudioPath is a path to a recorded audio file containing the user's
// question (any format ffmpeg can read). voice is the Kokoro voice name for
// the answer (e.g. "af_heart").
func Converse(
	store *db.Store,
	sttClient *stt.Client,
	ttsClient *tts.Client,
	rag *llm.RAG,
	workID int64,
	questionAudioPath string,
	voice string,
) (*ConverseResponse, error) {
	if sttClient == nil {
		return nil, fmt.Errorf("STT service not available")
	}
	if rag == nil || rag.Client() == nil {
		return nil, fmt.Errorf("LLM not configured")
	}

	// 1. Transcribe the question.
	questionResult, err := sttClient.TranscribeFile(questionAudioPath)
	if err != nil {
		return nil, fmt.Errorf("transcribe question: %w", err)
	}
	question := questionResult.Text
	if question == "" {
		return nil, fmt.Errorf("couldn't understand the question (empty transcription)")
	}

	// 2. Run RAG.
	answer, err := AskWithCitations(store, rag, workID, question)
	if err != nil {
		return nil, fmt.Errorf("ask: %w", err)
	}

	resp := &ConverseResponse{
		Question:  question,
		Answer:    answer.Text,
		Citations: answer.Citations,
		Model:     answer.Model,
	}

	// 3. TTS the answer (best-effort — conversation still returns text if TTS fails).
	if ttsClient != nil && voice != "" && answer.Text != "" {
		audioBytes, err := ttsClient.Synthesize(answer.Text, voice)
		if err == nil && len(audioBytes) > 0 {
			resp.AudioBase64 = base64.StdEncoding.EncodeToString(audioBytes)
			resp.AudioMIME = "audio/mpeg"
		}
	}

	return resp, nil
}

// SaveUploadedAudio writes an uploaded audio blob to a temp file for STT.
// Returns the path — caller is responsible for cleanup.
func SaveUploadedAudio(content []byte, ext string) (string, error) {
	if ext == "" {
		ext = "webm"
	}
	tmp, err := os.CreateTemp("", "converse-*."+ext)
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	return filepath.Clean(tmp.Name()), nil
}
