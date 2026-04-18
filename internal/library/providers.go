// Provider factory. Creates TTS + STT providers based on user settings.
// Settings are stored in the settings table and configurable via the web UI.
//
// Provider selection (per user settings):
//   tts_provider: "kokoro" (default, local) | "openai" (BYOK)
//   stt_provider: "whisper" (default, local) | "openai" (BYOK)
//   openai_api_key: required for openai providers
//   kokoro_url: default http://localhost:8880
//   whisper_url: default http://localhost:5200
package library

import (
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

// CreateTTSProvider returns the configured TTS provider based on settings.
func CreateTTSProvider(store *db.Store, fallbackURL string) tts.Provider {
	settings, _ := store.GetAllSettings()
	provider := settings["tts_provider"]
	switch provider {
	case "openai":
		key := settings["openai_api_key"]
		if key != "" {
			return tts.NewOpenAIClient(key)
		}
	}
	// Default: local Kokoro
	url := settings["kokoro_url"]
	if url == "" {
		url = fallbackURL
	}
	if url == "" {
		url = "http://localhost:8880"
	}
	return tts.NewClient(url)
}

// CreateSTTProvider returns the configured STT provider based on settings.
func CreateSTTProvider(store *db.Store, fallbackURL string) stt.Provider {
	settings, _ := store.GetAllSettings()
	provider := settings["stt_provider"]
	switch provider {
	case "openai":
		key := settings["openai_api_key"]
		if key != "" {
			return stt.NewOpenAIClient(key)
		}
	}
	// Default: local faster-whisper
	url := settings["whisper_url"]
	if url == "" {
		url = fallbackURL
	}
	if url == "" {
		url = "http://localhost:5200"
	}
	return stt.NewClient(url)
}
