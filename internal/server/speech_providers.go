package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

// Test-connection endpoints for the speech providers (#54 step 2) — mirror
// handleTestLLM. They build a provider from the POSTED provider/key (falling
// back to stored settings for empty/masked fields) and run a real Health()
// probe: for cloud that validates the key via GET /v1/models; for local it hits
// the engine's health endpoint. Always HTTP 200 with an {ok,...} body.

type speechTestBody struct {
	Provider string `json:"provider"`
	APIKey   string `json:"api_key"`
}

// resolveKey returns the posted key, or (when empty/masked) the stored per-engine
// key, then the shared openai_api_key.
func (s *Server) resolveSpeechKey(posted string, settings map[string]string, perEngineKey string) string {
	if posted != "" && !isMaskedSecret(posted) {
		return posted
	}
	if v := settings[perEngineKey]; v != "" {
		return v
	}
	return settings["openai_api_key"]
}

func (s *Server) handleTestTTS(w http.ResponseWriter, r *http.Request) {
	var body speechTestBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	settings, _ := s.store.GetAllSettings()
	provider := body.Provider
	if provider == "" {
		provider = settings["tts_provider"]
	}
	var p interface {
		Name() string
		Health() error
	}
	if provider == "openai" {
		key := s.resolveSpeechKey(body.APIKey, settings, "tts_api_key")
		if key == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no API key set"})
			return
		}
		p = tts.NewOpenAIClient(key)
	} else {
		url := settings["kokoro_url"]
		if url == "" {
			url = s.TTSURL
		}
		if url == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no local TTS engine configured"})
			return
		}
		p = tts.NewClient(url)
	}
	writeSpeechTestResult(w, p)
}

func (s *Server) handleTestSTT(w http.ResponseWriter, r *http.Request) {
	var body speechTestBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	settings, _ := s.store.GetAllSettings()
	provider := body.Provider
	if provider == "" {
		provider = settings["stt_provider"]
	}
	var p interface {
		Name() string
		Health() error
	}
	if provider == "openai" {
		key := s.resolveSpeechKey(body.APIKey, settings, "stt_api_key")
		if key == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no API key set"})
			return
		}
		p = stt.NewOpenAIClient(key)
	} else {
		url := settings["whisper_url"]
		if url == "" {
			url = s.STTURL
		}
		if url == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no local STT engine configured"})
			return
		}
		p = stt.NewClient(url)
	}
	writeSpeechTestResult(w, p)
}

func writeSpeechTestResult(w http.ResponseWriter, p interface {
	Name() string
	Health() error
}) {
	start := time.Now()
	err := p.Health()
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "provider": p.Name(), "latency_ms": latency})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": p.Name(), "latency_ms": latency})
}
