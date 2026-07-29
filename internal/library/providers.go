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

// CreateTTSProvider returns the configured TTS provider, or nil if none is
// configured. Cloud (BYOK) when tts_provider="openai" + a key; otherwise the
// local Kokoro engine at the settings url, else the fallback (the -tts-url flag
// / ABOOKIFY_TTS_URL env). nil when nothing is set — same as the pre-provider
// behaviour where an empty URL meant "no TTS".
func CreateTTSProvider(store *db.Store, fallbackURL string) tts.Provider {
	settings, _ := store.GetAllSettings()
	if settings["tts_provider"] == "openai" {
		if key := settings["openai_api_key"]; key != "" {
			return tts.NewOpenAIClient(key)
		}
	}
	url := settings["kokoro_url"]
	if url == "" {
		url = fallbackURL
	}
	if url == "" {
		return nil
	}
	return tts.NewClient(url)
}

// CreateSTTProvider returns the configured STT provider, or nil if none is
// configured (see CreateTTSProvider).
func CreateSTTProvider(store *db.Store, fallbackURL string) stt.Provider {
	settings, _ := store.GetAllSettings()
	if settings["stt_provider"] == "openai" {
		if key := settings["openai_api_key"]; key != "" {
			return stt.NewOpenAIClient(key)
		}
	}
	url := settings["whisper_url"]
	if url == "" {
		url = fallbackURL
	}
	if url == "" {
		return nil
	}
	return stt.NewClient(url)
}
