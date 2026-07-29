package server

import (
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestMigrateLegacyCredentials(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Seed legacy settings mirroring PJ: OpenAI llm key + Gemini voice key,
	// empty tts, a local stt provider (must be skipped — no credential).
	store.SetSetting("llm_provider", "openai")
	store.SetSetting("llm_api_key", "sk-proj-REALKEY123456")
	store.SetSetting("voice_provider", "gemini")
	store.SetSetting("voice_api_key", "AQ.AbGoogleKey7890")
	store.SetSetting("stt_provider", "whisper")
	store.SetSetting("stt_api_key", "")

	created, err := MigrateLegacyCredentials(store)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := map[string]bool{}
	for _, v := range created {
		got[v] = true
	}
	if !got["openai"] || !got["google"] || len(created) != 2 {
		t.Fatalf("expected openai+google created, got %v", created)
	}

	creds, _ := store.ListCredentials()
	byVendor := map[string]db.Credential{}
	for _, c := range creds {
		byVendor[c.Provider] = c
	}
	if byVendor["openai"].Fields["api_key"] != "sk-proj-REALKEY123456" {
		t.Fatalf("openai key not migrated: %q", byVendor["openai"].Fields["api_key"])
	}
	// The old "gemini" voice provider maps to the "google" vendor.
	if byVendor["google"].Fields["api_key"] != "AQ.AbGoogleKey7890" {
		t.Fatalf("gemini→google key not migrated: %q", byVendor["google"].Fields["api_key"])
	}
	if _, ok := byVendor["whisper"]; ok {
		t.Fatal("a local (whisper) provider must not become a credential")
	}

	settings, _ := store.GetAllSettings()
	if settings["llm_credential_id"] == "" || settings["voice_credential_id"] == "" {
		t.Fatalf("credential ids not linked: llm=%q voice=%q", settings["llm_credential_id"], settings["voice_credential_id"])
	}

	// NON-DESTRUCTIVE: the legacy keys are left intact so the old path keeps
	// working until resolution switches over.
	if settings["llm_api_key"] != "sk-proj-REALKEY123456" || settings["voice_api_key"] != "AQ.AbGoogleKey7890" {
		t.Fatal("legacy keys were modified — migration must be non-destructive")
	}

	// IDEMPOTENT: a second run creates nothing new and adds no duplicate rows.
	created2, _ := MigrateLegacyCredentials(store)
	if len(created2) != 0 {
		t.Fatalf("second run created %v (should be a no-op)", created2)
	}
	if creds2, _ := store.ListCredentials(); len(creds2) != 2 {
		t.Fatalf("idempotency broken: %d credentials after 2nd run", len(creds2))
	}
}
