package server

import "testing"

// findField locates a schema field by key across all groups.
func findField(doc *SettingsSchemaDoc, key string) *SettingsField {
	for gi := range doc.Groups {
		for fi := range doc.Groups[gi].Fields {
			if doc.Groups[gi].Fields[fi].Key == key {
				return &doc.Groups[gi].Fields[fi]
			}
		}
	}
	return nil
}

func optionValues(f *SettingsField) map[string]bool {
	m := map[string]bool{}
	for _, o := range f.Options {
		m[o.Value] = true
	}
	return m
}

func TestApplyProviderEligibility(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Stored, verified credentials mirroring PJ's real state:
	//   openai   → llm, stt, tts
	//   google   → llm, voice   (never tts — the Google Cloud TTS caveat)
	//   anthropic→ llm
	mustCred := func(provider string, caps []string) {
		id, err := s.store.UpsertProviderCredential(provider, "", map[string]string{"api_key": "k"})
		if err != nil {
			t.Fatalf("upsert %s: %v", provider, err)
		}
		if err := s.store.SetCredentialCapabilities(id, caps); err != nil {
			t.Fatalf("caps %s: %v", provider, err)
		}
	}
	mustCred("openai", []string{"llm", "stt", "tts"})
	mustCred("google", []string{"llm", "voice"})
	mustCred("anthropic", []string{"llm"})

	doc := SettingsSchema()
	s.applyProviderEligibility(&doc)

	llm := optionValues(findField(&doc, "llm_provider"))
	for _, want := range []string{"", "anthropic", "google", "openai", "ollama"} {
		if !llm[want] {
			t.Errorf("llm provider options missing %q: %v", want, llm)
		}
	}

	stt := optionValues(findField(&doc, "stt_provider"))
	if !stt["openai"] {
		t.Errorf("stt should offer openai: %v", stt)
	}
	for _, notWant := range []string{"google", "anthropic"} {
		if stt[notWant] {
			t.Errorf("stt must not offer %q (not stt-capable/integrated): %v", notWant, stt)
		}
	}

	// The caveat, enforced by data: Google (verified [llm,voice], never tts)
	// must NOT appear under Text-to-Speech. Only openai (verified tts) does.
	tts := optionValues(findField(&doc, "tts_provider"))
	if !tts["openai"] {
		t.Errorf("tts should offer openai: %v", tts)
	}
	if tts["google"] {
		t.Fatal("tts must never offer google — a Gemini key can't reach Google Cloud TTS")
	}
	if tts["anthropic"] {
		t.Errorf("tts must not offer anthropic: %v", tts)
	}
}

// A credential that verifies nothing (empty capabilities) lights up no lane.
func TestApplyProviderEligibility_UnverifiedOffersNothing(t *testing.T) {
	s, _, _ := newTestServer(t)
	if _, err := s.store.UpsertProviderCredential("openai", "", map[string]string{"api_key": "k"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc := SettingsSchema()
	s.applyProviderEligibility(&doc)
	if optionValues(findField(&doc, "stt_provider"))["openai"] {
		t.Fatal("an unverified openai credential must not appear in the STT lane")
	}
}
