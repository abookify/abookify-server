package library

import "testing"

// TestProviderResolution_CloudVsLocal proves the "add a key, run no local engine"
// switch routes correctly: selecting OpenAI resolves to the CLOUD client when a
// key is available (vault, then legacy), and falls back to LOCAL when the setting
// is default OR when OpenAI is selected but no key exists (never route to a
// keyless cloud). This is the exact resolution ReloadSpeech runs, so a POST that
// flips stt_provider/tts_provider takes effect through this path.
func TestProviderResolution_CloudVsLocal(t *testing.T) {
	store := testStoreForLib(t)

	// Default (no provider selected) → local engines.
	if p := CreateSTTProvider(store, "http://localhost:5200"); p == nil || p.Name() != "whisper-local" {
		t.Fatalf("default STT should be local whisper, got %v", nameOf(p))
	}
	if p := CreateTTSProvider(store, "http://localhost:8880"); p == nil || p.Name() != "kokoro" {
		t.Fatalf("default TTS should be local kokoro, got %v", nameOf(p))
	}

	// OpenAI selected but NO key anywhere → must fall back to local (can't route
	// to a keyless cloud, and silently doing nothing would be worse).
	if err := store.SetSetting("stt_provider", "openai"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting("tts_provider", "openai"); err != nil {
		t.Fatal(err)
	}
	if got := CreateSTTProvider(store, "http://localhost:5200").Name(); got != "whisper-local" {
		t.Fatalf("openai selected + no key → local fallback expected, got %s", got)
	}
	if got := CreateTTSProvider(store, "http://localhost:8880").Name(); got != "kokoro" {
		t.Fatalf("openai selected + no key → local fallback expected, got %s", got)
	}

	// Add an OpenAI credential to the VAULT → now both lanes route to the cloud
	// client (the "run no local engine" case for a GPU-less install).
	if _, err := store.UpsertProviderCredential("openai", "", map[string]string{"api_key": "sk-test-vault"}); err != nil {
		t.Fatalf("upsert credential: %v", err)
	}
	if got := CreateSTTProvider(store, "http://localhost:5200").Name(); got != "openai-whisper" {
		t.Fatalf("openai + vault key → cloud STT expected, got %s", got)
	}
	if got := CreateTTSProvider(store, "http://localhost:8880").Name(); got != "openai-tts" {
		t.Fatalf("openai + vault key → cloud TTS expected, got %s", got)
	}
}

func nameOf(p interface{ Name() string }) string {
	if p == nil {
		return "<nil>"
	}
	return p.Name()
}
