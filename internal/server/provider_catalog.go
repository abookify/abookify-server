package server

// ProviderCredentialField declares one input a vendor's credential needs. The
// Keys section renders these, so a multi-part credential (Azure: api_key +
// region + deployment; AWS: access key + secret + region) declares its own shape
// here rather than the UI hardcoding a single "API key" box.
type ProviderCredentialField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ProviderDescriptor is one vendor the user can add a credential for. ONE entry
// per vendor: a single key lights up every feature (Kind) that vendor can serve,
// once the specific credential is VERIFIED for it (db.Credential.Capabilities —
// declaring a Kind here only means the vendor CAN serve it; selectors gate on the
// per-credential verified set). PolicyURL is the vendor's OWN privacy-policy link
// — we link it and say "their terms govern", never summarise it. See
// docs/settings-credentials-restructure.md.
type ProviderDescriptor struct {
	ID               string                    `json:"id"`
	Label            string                    `json:"label"`
	Kinds            []string                  `json:"kinds"` // "llm" | "stt" | "tts" | "voice"
	CredentialFields []ProviderCredentialField `json:"credential_fields"`
	PolicyURL        string                    `json:"policy_url"`
}

// ProviderCatalog is the source of truth for the Keys section (and, later, the
// per-feature provider/model selectors). Kept small to start — the vendors PJ
// has keys for plus the LLM vendors already wired — and grows as integrations
// land. Azure/AWS (multi-field credentials) are declared once their integrations
// exist so the form never offers a key it can't yet consume.
func ProviderCatalog() []ProviderDescriptor {
	apiKey := ProviderCredentialField{Key: "api_key", Label: "API key", Placeholder: "paste your key", Secret: true, Required: true}
	return []ProviderDescriptor{
		{
			ID: "openai", Label: "OpenAI", Kinds: []string{"llm", "stt", "tts"},
			CredentialFields: []ProviderCredentialField{apiKey},
			PolicyURL:        "https://openai.com/policies/privacy-policy/",
		},
		{
			ID: "google", Label: "Google (Gemini)", Kinds: []string{"llm", "voice"},
			CredentialFields: []ProviderCredentialField{apiKey},
			PolicyURL:        "https://policies.google.com/privacy",
		},
		{
			ID: "anthropic", Label: "Anthropic (Claude)", Kinds: []string{"llm"},
			CredentialFields: []ProviderCredentialField{apiKey},
			PolicyURL:        "https://www.anthropic.com/legal/privacy",
		},
	}
}

// providerDescriptor looks up a vendor by id.
func providerDescriptor(id string) (ProviderDescriptor, bool) {
	for _, p := range ProviderCatalog() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderDescriptor{}, false
}

// fieldIsSecret reports whether a vendor's credential field is a secret (masked
// on read). Unknown fields default to secret — never leak by omission.
func fieldIsSecret(desc ProviderDescriptor, key string) bool {
	for _, f := range desc.CredentialFields {
		if f.Key == key {
			return f.Secret
		}
	}
	return true
}
