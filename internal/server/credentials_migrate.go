package server

import (
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// legacyVendor maps an old per-feature provider value to a catalog vendor id.
// The old voice-conversation slot used "gemini", which is Google's vendor key.
// Local engines / unknown values return "" (no credential to migrate).
func legacyVendor(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "openai":
		return "openai"
	case "gemini", "google":
		return "google"
	case "anthropic":
		return "anthropic"
	default:
		return "" // kokoro / whisper / ollama / "" — local or no key
	}
}

// MigrateLegacyCredentials copies BYOK keys from the old per-feature settings
// (llm/stt/tts/voice _api_key + _provider) into the credentials table so an
// existing user's keys appear in the Keys section WITHOUT re-entering them.
//
// NON-DESTRUCTIVE and idempotent: the legacy settings are left intact (they
// still drive features until provider-resolution switches over), a vendor that
// already has a credential is never clobbered, and re-running is a no-op.
// Returns the vendor ids it created a credential for.
func MigrateLegacyCredentials(store *db.Store) ([]string, error) {
	settings, err := store.GetAllSettings()
	if err != nil {
		return nil, err
	}
	existing, err := store.ListCredentials()
	if err != nil {
		return nil, err
	}
	credForVendor := map[string]int64{}
	for _, c := range existing {
		if _, ok := credForVendor[c.Provider]; !ok {
			credForVendor[c.Provider] = c.ID
		}
	}

	lanes := []struct{ providerKey, apiKeyKey, credIDKey string }{
		{"llm_provider", "llm_api_key", "llm_credential_id"},
		{"stt_provider", "stt_api_key", "stt_credential_id"},
		{"tts_provider", "tts_api_key", "tts_credential_id"},
		{"voice_provider", "voice_api_key", "voice_credential_id"},
	}

	var created []string
	for _, ln := range lanes {
		vendor := legacyVendor(settings[ln.providerKey])
		key := strings.TrimSpace(settings[ln.apiKeyKey])
		if vendor == "" || key == "" {
			continue
		}
		if _, ok := providerDescriptor(vendor); !ok {
			continue // vendor not in the catalog yet
		}
		id, ok := credForVendor[vendor]
		if !ok {
			// No credential for this vendor yet — create one from the legacy key.
			newID, err := store.UpsertProviderCredential(vendor, "", map[string]string{"api_key": key})
			if err != nil {
				return created, err
			}
			credForVendor[vendor] = newID
			id = newID
			created = append(created, vendor)
		}
		// Link the feature to the vendor's credential if not already linked
		// (additive; provider-resolution consumes *_credential_id later).
		if settings[ln.credIDKey] == "" {
			_ = store.SetSetting(ln.credIDKey, strconv.FormatInt(id, 10))
		}
	}
	return created, nil
}
