package server

import "strings"

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
//	library_roots    marker: client renders the library-roots widget from
//	                 /api/library/roots (no flat KV value) (#220)
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

// voiceLabel returns the friendly display name for a Kokoro voice id (just the
// first word of the catalog label, e.g. "af_heart" → "Heart"), defaulting to
// Heart when the id is empty/unknown. Used to name a generated TTS edition.
func voiceLabel(voice string) string {
	if voice == "" || strings.HasPrefix(voice, "en_US") {
		voice = "af_heart"
	}
	for _, g := range kokoroVoiceGroups {
		for _, o := range g.Options {
			if o.Value == voice {
				return strings.Fields(o.Label)[0] // "Heart (default, natural)" → "Heart"
			}
		}
	}
	return voice
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
				Description: "Voice used when generating audiobooks from ebook text. Use the local Kokoro engine (default), or add a cloud key to narrate without a local engine.",
				Fields: []SettingsField{
					{
						Key: "tts_provider", Label: "Engine", Type: "select", Default: "",
						Options: []SettingsOption{
							{"", "Local engine — Kokoro (default)"},
							{"openai", "OpenAI (cloud, bring your own key)"},
						},
						Help: "Local runs on this machine (no key, no cloud). OpenAI needs a key below and works with no local engine installed.",
					},
					{
						Key: "tts_api_key", Label: "OpenAI API key", Type: "secret", Secret: true,
						Placeholder: "sk-… (paste to replace, leave empty to keep)",
						Addons:      []string{"test", "clear"},
					},
					{
						Key: "tts_voice", Label: "Voice", Type: "select_or_custom", Default: "af_heart",
						OptionsEndpoint: "/api/tts/voices", DependsOn: "tts_provider", AllowCustom: false,
						Addons: []string{"preview"},
						Help:   "Voice choices depend on the engine above. Preview generates a short sample (local Kokoro only).",
					}},
			},
			{
				Key:         "stt",
				Title:       "Speech-to-Text Model",
				Description: "Engine + model for transcribing audiobooks to text. Use the local Whisper engine (default), or add a cloud key to transcribe without a local engine. Changes apply to new jobs.",
				Fields: []SettingsField{
					{
						Key: "stt_provider", Label: "Engine", Type: "select", Default: "",
						Options: []SettingsOption{
							{"", "Local engine — Whisper (default)"},
							{"openai", "OpenAI (cloud, bring your own key)"},
						},
						Help: "Local runs on this machine (no key, no cloud). OpenAI Whisper needs a key below and works with no local engine installed.",
					},
					{
						Key: "stt_api_key", Label: "OpenAI API key", Type: "secret", Secret: true,
						Placeholder: "sk-… (paste to replace, leave empty to keep)",
						Addons:      []string{"test", "clear"},
					},
					{
						Key: "stt_model", Label: "Model", Type: "select_or_custom", Default: "large-v3",
						OptionsEndpoint: "/api/stt/models", DependsOn: "stt_provider", AllowCustom: false,
						Help: "Model choices depend on the engine above. OpenAI offers only Whisper v2, the model whose word-level timestamps are verified for sync.",
					},
					{
						Key: "stt_compute_mode", Label: "Compute device", Type: "select", Default: "auto",
						Options: []SettingsOption{
							{"auto", "Auto-detect (GPU if available, else CPU)"},
							{"gpu", "GPU (fastest, needs an NVIDIA GPU)"},
							{"cpu", "CPU (works everywhere, slower)"},
						},
						Help: "Which device transcription runs on. Auto uses a GPU when one is present and falls back to CPU otherwise. Applies when the transcription engine (re)starts; the current device is shown below.",
					},
					{
						Key: "stt_idle_timeout", Label: "Unload the transcription model when idle", Type: "select", Default: "60",
						Options: []SettingsOption{
							{"5", "5 minutes"}, {"15", "15 minutes"}, {"30", "30 minutes"},
							{"60", "1 hour (default)"}, {"0", "Never (always loaded)"},
						},
						Help: "Frees the Whisper speech-to-text model (~3.2 GB RAM, or VRAM on GPU) after it's been idle this long; the next transcription reloads it automatically at a one-time cost of about 1–2 seconds (measured on GPU; ~3 s cold). This reclaims the transcription (STT) model ONLY — the Kokoro text-to-speech engine (~1.1 GB) has no unload endpoint, so it stays loaded regardless of this setting.",
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
					{
						Key: "qa_extract_only", Label: "Answer only from the book text (spoiler-safe)", Type: "bool", Default: "false",
						Help: "Answers quote the book's own words up to where you're reading, instead of the AI writing them — so a famous book the AI already knows can't reveal what's coming. Also turns off AI chapter summaries and recaps (those are written by the AI). One switch; it governs Q&A everywhere.",
					},
				},
			},
			{
				Key:   "voice",
				Title: "Voice Chat",
				// HONEST STATE (verified 2026-07-29): real-time speech-to-speech
				// (Gemini Live / OpenAI Realtime) is NOT wired up. A provider/key
				// saved here is stored but nothing consumes it for a conversation —
				// no server code reads voice_provider/voice_api_key for speech. (A
				// separate, working push-to-talk round-trip exists at
				// POST /api/works/{id}/converse using the local Whisper+Kokoro
				// engines, but it is not this feature and has no web/mobile client.)
				// Do not restore a "this works" description until the realtime path
				// is actually built.
				Description: "Coming soon — real-time voice conversation (Gemini Live / OpenAI Realtime) is not wired up yet. A provider and key saved here are stored but not yet used for a conversation.",
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
				Description:  "Detects a named cast of characters from a work's EPUB. Extract it from the cast panel on any ebook — it runs in-process in under a second, with nothing to install. Places and allusions can still surface, and aliases that share no tokens (Rodya / Raskolnikov) split into separate rows — hence the experimental label.",
			},
			{
				Key:         "library",
				Title:       "Library locations",
				Description: "The folders scanned for books (#220). Manage them via GET/POST/DELETE/PATCH /api/library/roots — the list carries per-root path, book count, and reachable/offline state. An unplugged drive is shown offline; its books are kept (stale), never deleted.",
				// A marker field: clients render the roots-management widget from the
				// roots API rather than a flat KV value (the schema has no list type).
				Fields: []SettingsField{{
					Key: "library_roots", Label: "Library folders", Type: "library_roots",
				}},
			},
		},
	}
}
