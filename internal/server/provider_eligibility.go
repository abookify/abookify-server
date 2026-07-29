package server

// Per-feature provider eligibility. The settings schema (#202) is the single
// source both web + mobile render from; here we inject the DYNAMIC provider
// option lists for the llm/stt/tts lanes so each dropdown offers only what a
// stored key can genuinely serve — no offer that fails at use.
//
// A vendor appears in a lane only when the intersection of two things includes
// that lane's kind:
//   - the credential's VERIFIED capabilities (probed, never assumed), and
//   - integratedKinds — what THIS server can actually consume today.
//
// That intersection is why a Gemini credential (verified [llm, voice]) never
// appears under Text-to-Speech: Google Cloud TTS is a separate product a Gemini
// key can't reach, so we never probe tts for it and never offer it. PJ's caveat,
// enforced by data rather than a hardcoded exception.

// integratedKinds reports which feature kinds this server can actually consume
// for a vendor today (a client/path exists). nil = no integration yet.
func integratedKinds(provider string) map[string]bool {
	switch provider {
	case "openai":
		return map[string]bool{"llm": true, "stt": true, "tts": true}
	case "anthropic":
		return map[string]bool{"llm": true}
	case "google":
		// Gemini LLM is wired (OpenAI-compatible client). Gemini voice + Google
		// Cloud TTS are not consumed as feature engines yet, so they stay out of
		// the selectors even where the credential verifies them.
		return map[string]bool{"llm": true}
	}
	return nil
}

// eligibleVendors returns the vendor ids that may be offered for a lane: a
// stored credential whose verified capabilities include kind AND that this
// server integrates for kind. Deterministic order (ListCredentials sorts by
// provider, id).
func (s *Server) eligibleVendors(kind string) []string {
	creds, err := s.store.ListCredentials()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range creds {
		if seen[c.Provider] {
			continue
		}
		integ := integratedKinds(c.Provider)
		if integ == nil || !integ[kind] {
			continue
		}
		for _, capab := range c.Capabilities {
			if capab == kind {
				out = append(out, c.Provider)
				seen[c.Provider] = true
				break
			}
		}
	}
	return out
}

// providerLabel is the display name for a vendor from the catalog, or the id.
func providerLabel(id string) string {
	if d, ok := providerDescriptor(id); ok {
		return d.Label
	}
	return id
}

// applyProviderEligibility rewrites the llm/stt/tts provider dropdown options in
// a copy of the schema so each lane offers its always-available (local/off)
// choices plus only the credential-eligible cloud vendors. The currently-saved
// provider is always kept in the list even if it's since become ineligible, so
// the UI reflects the real selection instead of silently snapping to the head.
func (s *Server) applyProviderEligibility(doc *SettingsSchemaDoc) {
	settings, _ := s.store.GetAllSettings()

	// Head/tail options that don't come from the credentials vault, per lane.
	heads := map[string]SettingsOption{
		"llm_provider": {"", "Not configured (keyword search only)"},
		"stt_provider": {"", "Local engine — Whisper (default)"},
		"tts_provider": {"", "Local engine — Kokoro (default)"},
	}
	tails := map[string][]SettingsOption{
		// LLM keeps its keyless-local + legacy-inline-key choices.
		"llm_provider": {
			{"ollama", "Ollama (local, no key)"},
			{"openrouter", "OpenRouter (bring your own key)"},
		},
	}
	kindOf := map[string]string{"llm_provider": "llm", "stt_provider": "stt", "tts_provider": "tts"}

	for gi := range doc.Groups {
		for fi := range doc.Groups[gi].Fields {
			f := &doc.Groups[gi].Fields[fi]
			kind, ok := kindOf[f.Key]
			if !ok {
				continue
			}
			opts := []SettingsOption{heads[f.Key]}
			present := map[string]bool{"": true}
			for _, v := range s.eligibleVendors(kind) {
				opts = append(opts, SettingsOption{Value: v, Label: providerLabel(v)})
				present[v] = true
			}
			for _, t := range tails[f.Key] {
				opts = append(opts, t)
				present[t.Value] = true
			}
			// Preserve the saved selection if it's no longer eligible, so the UI
			// shows what's actually configured (rather than snapping to head).
			if cur := settings[f.Key]; cur != "" && !present[cur] {
				opts = append(opts, SettingsOption{Value: cur, Label: providerLabel(cur) + " (saved; key no longer verified)"})
			}
			f.Options = opts
		}
	}
}
