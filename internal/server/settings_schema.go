package server

// Backend-driven settings schema (#202). Web AND mobile had their settings
// UIs hardcoded against the flat /api/settings KV and drifted; this is the
// single source of truth they both render from. The schema describes the
// field set — types, options, grouping, secret-masking, help — while the
// VALUES still flow through the existing GET/POST /api/settings KV (secrets
// masked on read, merged on write). Operations that aren't config (QR
// pairing, disk usage, rescan, exports, cover fetch) are NOT in the schema —
// they're actions, not settings, and stay client-owned.
//
// Versioned: bump SettingsSchemaVersion on a breaking shape change so clients
// can detect drift. Contract documented in ../handoff/server-web.md.

// SettingsSchemaVersion is the schema shape version. Additive changes (new
// field/group/option) keep the version; a breaking change (renamed type,
// removed field semantics) bumps it.
const SettingsSchemaVersion = 1

// SettingsOption is one choice for a select field.
type SettingsOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// SettingsOptionGroup is an <optgroup> — a labeled cluster of options (e.g.
// "Female · American English" voices).
type SettingsOptionGroup struct {
	Label   string           `json:"label"`
	Options []SettingsOption `json:"options"`
}

// SettingsField describes one setting. Type drives the input widget:
//
//	text             single-line text
//	secret           password input; value is masked on read + write-to-keep
//	bool             checkbox ("true"/"false" string in the KV)
//	select           dropdown from Options or OptionGroups
//	select_or_custom dropdown (OptionsEndpoint) with a free-text fallback
//
// Addons name client-known adjunct controls for the field ("preview" a TTS
// voice, "test" an LLM connection). Clients that don't implement an addon
// simply skip it.
type SettingsField struct {
	Key          string                `json:"key"`
	Label        string                `json:"label"`
	Type         string                `json:"type"`
	Help         string                `json:"help,omitempty"`
	Placeholder  string                `json:"placeholder,omitempty"`
	Default      string                `json:"default,omitempty"`
	Secret       bool                  `json:"secret,omitempty"`     // masked on read (isSecretSettingKey)
	WriteOnly    bool                  `json:"write_only,omitempty"` // never returned by GET (e.g. password)
	Options      []SettingsOption      `json:"options,omitempty"`
	OptionGroups []SettingsOptionGroup `json:"option_groups,omitempty"`
	// OptionsEndpoint is a GET that returns the dynamic option list (LLM models
	// per provider). DependsOn names the field whose value parameterizes it.
	OptionsEndpoint string   `json:"options_endpoint,omitempty"`
	DependsOn       string   `json:"depends_on,omitempty"`
	AllowCustom     bool     `json:"allow_custom,omitempty"`
	Addons          []string `json:"addons,omitempty"`
}

// SettingsGroup is a titled section of related fields.
type SettingsGroup struct {
	Key          string          `json:"key"`
	Title        string          `json:"title"`
	Description  string          `json:"description,omitempty"`
	Experimental bool            `json:"experimental,omitempty"`
	Fields       []SettingsField `json:"fields"`
}

// SettingsSchemaDoc is the GET /api/settings/schema payload.
type SettingsSchemaDoc struct {
	Version int             `json:"version"`
	Groups  []SettingsGroup `json:"groups"`
}

// kokoroVoiceGroups mirrors the Kokoro voice catalog the TTS service exposes.
var kokoroVoiceGroups = []SettingsOptionGroup{
	{Label: "Female · American English", Options: []SettingsOption{
		{"af_heart", "Heart (default, natural)"}, {"af_bella", "Bella"}, {"af_nicole", "Nicole"},
		{"af_sarah", "Sarah"}, {"af_nova", "Nova"}, {"af_sky", "Sky"}, {"af_river", "River"}, {"af_jessica", "Jessica"},
	}},
	{Label: "Male · American English", Options: []SettingsOption{
		{"am_adam", "Adam"}, {"am_michael", "Michael"}, {"am_eric", "Eric"}, {"am_liam", "Liam"}, {"am_puck", "Puck"},
	}},
	{Label: "Female · British English", Options: []SettingsOption{
		{"bf_emma", "Emma"}, {"bf_lily", "Lily"}, {"bf_alice", "Alice"},
	}},
	{Label: "Male · British English", Options: []SettingsOption{
		{"bm_george", "George"}, {"bm_daniel", "Daniel"}, {"bm_lewis", "Lewis"},
	}},
}

// SettingsSchema returns the canonical settings schema (#202). Static — the
// option lists are stable; the one dynamic list (LLM models per provider) is
// referenced by OptionsEndpoint so clients fetch it for the chosen provider.
func SettingsSchema() SettingsSchemaDoc {
	return SettingsSchemaDoc{
		Version: SettingsSchemaVersion,
		Groups: []SettingsGroup{
			{
				Key:         "tts",
				Title:       "Text-to-Speech Voice",
				Description: "Voice used when generating audiobooks from ebook text. All voices are powered by Kokoro and run locally.",
				Fields: []SettingsField{{
					Key: "tts_voice", Label: "Voice", Type: "select", Default: "af_heart",
					OptionGroups: kokoroVoiceGroups, Addons: []string{"preview"},
					Help: "Generates a short sample with the selected voice. Requires Kokoro to be running.",
				}},
			},
			{
				Key:         "stt",
				Title:       "Speech-to-Text Model",
				Description: "Whisper model used when transcribing audiobooks to text. Larger models are more accurate but slower. Changes apply to new transcription jobs.",
				Fields: []SettingsField{
					{
						Key: "stt_model", Label: "Model", Type: "select", Default: "large-v3",
						Options: []SettingsOption{
							{"large-v3", "Large V3 (best quality, slowest)"},
							{"medium", "Medium (good quality, moderate speed)"},
							{"small", "Small (decent quality, fast)"},
							{"base", "Base (basic quality, fastest)"},
						},
					},
					{
						Key: "stt_idle_timeout", Label: "Unload from memory after idle", Type: "select", Default: "60",
						Options: []SettingsOption{
							{"5", "5 minutes"}, {"15", "15 minutes"}, {"30", "30 minutes"},
							{"60", "1 hour (default)"}, {"0", "Never (always loaded)"},
						},
						Help: "The Whisper model uses ~3 GB of RAM (or VRAM on GPU). Unloading frees memory when not actively transcribing.",
					},
				},
			},
			{
				Key:         "llm",
				Title:       "Book Q&A",
				Description: "Add your own AI API key to enable intelligent question and answer about your books. Your key is stored locally on this server and only sent to the provider you select.",
				Fields: []SettingsField{
					{
						Key: "llm_provider", Label: "Provider", Type: "select", Default: "",
						Options: []SettingsOption{
							{"", "Not configured (keyword search only)"},
							{"anthropic", "Anthropic (Claude)"},
							{"openai", "OpenAI (ChatGPT)"},
							{"openrouter", "OpenRouter (Claude, GPT, Gemini, Llama, …)"},
							{"ollama", "Ollama (free, runs locally)"},
						},
					},
					{
						Key: "llm_api_key", Label: "API Key", Type: "secret", Secret: true,
						Placeholder: "sk-… (paste to replace, leave empty to keep)",
						Addons:      []string{"test", "clear"},
					},
					{
						Key: "llm_model", Label: "Model", Type: "select_or_custom",
						OptionsEndpoint: "/api/llm/models", DependsOn: "llm_provider", AllowCustom: true,
						Help: "Pick a model — switch to a more capable one for better answers.",
					},
					{
						Key: "llm_base_url", Label: "Base URL (optional, for proxies or self-hosted)", Type: "text",
						Placeholder: "Leave blank for default",
					},
				},
			},
			{
				Key:         "voice",
				Title:       "Voice Chat",
				Description: "Talk to your books using real-time voice conversation. Requires a speech-to-speech API key. This feature sends audio to an external service.",
				Fields: []SettingsField{
					{
						Key: "voice_provider", Label: "Provider", Type: "select", Default: "",
						Options: []SettingsOption{
							{"", "Not configured"},
							{"gemini", "Google Gemini Live"},
							{"openai-realtime", "OpenAI Realtime"},
						},
					},
					{
						Key: "voice_api_key", Label: "API Key", Type: "secret", Secret: true,
						Placeholder: "paste to replace, leave empty to keep", Addons: []string{"clear"},
					},
				},
			},
			{
				Key:         "security",
				Title:       "Security",
				Description: "Optional but recommended — without a password, anyone who can reach your server's public URL can read your library, stream audio, and use the AI features.",
				Fields: []SettingsField{
					{Key: "auth_username", Label: "Username", Type: "text", Placeholder: "pj"},
					{
						Key: "auth_password", Label: "Password", Type: "secret", Secret: true, WriteOnly: true,
						Placeholder: "set a password to protect this server",
						Help:        "Leave blank to keep the current password, or type a new one to change it.",
					},
				},
			},
			{
				Key:          "cast",
				Title:        "Cast of Characters",
				Experimental: true,
				Description:  "Detects a named cast of characters from a work's EPUB. Enable it from the cast panel on any ebook — the server downloads (~6.5 GB) and runs the engine on demand, then stops it after it's idle to free memory. Extraction quality varies by genre/translation and aliases may over-split — hence the experimental label.",
				Fields: []SettingsField{{
					Key: "booknlp_enabled", Label: "Enable BookNLP cast extraction", Type: "bool", Default: "false",
				}},
			},
		},
	}
}
