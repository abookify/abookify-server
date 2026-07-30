package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVoiceSessionOutboundBoundary_NoBookText enforces the privacy stance in
// code: minting a realtime session sends the provider ONLY session config
// (type + model) — never any book or library text — and the real API key is
// used server-side only (the caller gets an ephemeral token, not the key).
// If a future change smuggles book context or the real key into the mint, this
// fails.
func TestVoiceSessionOutboundBoundary_NoBookText(t *testing.T) {
	var capturedBody, capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/realtime/client_secrets") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": "ek_test_ephemeral", "expires_at": 123})
	}))
	defer srv.Close()

	tok, exp, err := mintRealtimeToken(srv.Client(), srv.URL, "sk-REAL-secret-key", "gpt-realtime")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "ek_test_ephemeral" || exp != 123 {
		t.Fatalf("unexpected mint result: token=%q exp=%d", tok, exp)
	}

	// The outbound body is EXACTLY session config — parse it and assert only the
	// allowed keys are present, and no book/library text.
	var body map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &body); err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	sess, ok := body["session"].(map[string]any)
	if !ok {
		t.Fatalf("expected a session object, got: %s", capturedBody)
	}
	for k := range body {
		if k != "session" {
			t.Fatalf("PRIVACY BOUNDARY: unexpected top-level field %q in mint request: %s", k, capturedBody)
		}
	}
	for k := range sess {
		if k != "type" && k != "model" {
			t.Fatalf("PRIVACY BOUNDARY: unexpected session field %q (only type+model may leave): %s", k, capturedBody)
		}
	}
	if sess["type"] != "realtime" || sess["model"] != "gpt-realtime" {
		t.Fatalf("session config wrong: %v", sess)
	}

	// The REAL key is used server-side in the Authorization header, and the
	// caller receives the EPHEMERAL token — never the real key.
	if capturedAuth != "Bearer sk-REAL-secret-key" {
		t.Fatalf("real key should authorize the mint, got %q", capturedAuth)
	}
	if strings.Contains(tok, "sk-REAL") {
		t.Fatal("PRIVACY BOUNDARY: the real key must never be returned to the caller")
	}
}

// TestRealtimeSessionConfig_MinimalShape locks the exact minted payload shape.
func TestRealtimeSessionConfig_MinimalShape(t *testing.T) {
	cfg := realtimeSessionConfig("gpt-realtime")
	sess := cfg["session"].(map[string]any)
	if len(cfg) != 1 || len(sess) != 2 || sess["type"] != "realtime" || sess["model"] != "gpt-realtime" {
		t.Fatalf("session config drifted from the pinned minimal shape: %v", cfg)
	}
}

// TestGeminiLiveMint_NoBookText holds the SAME boundary for the Google/Gemini
// Live provider: the mint body carries no book/library text, the real key stays
// in the query server-side, and the caller gets only the ephemeral token (name).
func TestGeminiLiveMint_NoBookText(t *testing.T) {
	if len(geminiLiveTokenConfig()) != 0 {
		t.Fatalf("gemini mint config must be empty (no book/library text), got %v", geminiLiveTokenConfig())
	}
	var capturedBody, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1alpha/auth_tokens") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "auth_tokens/ephemeral-xyz"})
	}))
	defer srv.Close()

	tok, err := mintGeminiLiveToken(srv.Client(), srv.URL, "AIza-REAL-google-key")
	if err != nil {
		t.Fatalf("gemini mint: %v", err)
	}
	if tok != "auth_tokens/ephemeral-xyz" {
		t.Fatalf("expected the ephemeral token name, got %q", tok)
	}
	if strings.TrimSpace(capturedBody) != "{}" {
		t.Fatalf("PRIVACY BOUNDARY: gemini mint body must be empty session config, got %q", capturedBody)
	}
	if !strings.Contains(capturedQuery, "AIza-REAL-google-key") {
		t.Fatalf("real key should authorize the mint via query, got %q", capturedQuery)
	}
	if strings.Contains(tok, "AIza-REAL") {
		t.Fatal("PRIVACY BOUNDARY: the real key must never be returned to the caller")
	}
}

// TestVoiceAvailable_GoogleGatedOnVoiceCapability honours the credential-capability
// declaration: a Google key is offered voice ONLY when it verified "voice"
// (Gemini Live), never merely because a key exists — so a Gemini key can't imply
// a capability (e.g. Google Cloud TTS, which it does not verify) it can't serve.
func TestVoiceAvailable_GoogleGatedOnVoiceCapability(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// A Google key that verified only [llm] (NOT voice) must NOT be offered voice.
	id, err := store.UpsertProviderCredential("google", "", map[string]string{"api_key": "AIza-x"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetCredentialCapabilities(id, []string{"llm"}); err != nil {
		t.Fatalf("caps: %v", err)
	}
	if srv.credentialHasCapability("google", "voice") {
		t.Fatal("google must NOT be voice-eligible without a verified voice capability")
	}

	// Once it verifies [llm, voice] (Gemini Live), it becomes eligible.
	if err := store.SetCredentialCapabilities(id, []string{"llm", "voice"}); err != nil {
		t.Fatalf("caps: %v", err)
	}
	if !srv.credentialHasCapability("google", "voice") {
		t.Fatal("google should be voice-eligible once it verified the voice capability")
	}
	// It must never be considered tts-capable (Google Cloud TTS is separate).
	if srv.credentialHasCapability("google", "tts") {
		t.Fatal("a Gemini key must never claim tts — Google Cloud TTS is a separate product")
	}
}

// TestDeepgramMint_NoBookText holds the SAME privacy boundary for Deepgram: the
// grant body carries ONLY a TTL (no book/library text), the real key authorizes
// server-side (Token header), and the caller gets only the temporary token.
func TestDeepgramMint_NoBookText(t *testing.T) {
	cfg := deepgramGrantConfig()
	for k := range cfg {
		if k != "ttl_seconds" {
			t.Fatalf("PRIVACY BOUNDARY: deepgram grant config may carry only ttl_seconds, got field %q", k)
		}
	}
	var capturedBody, capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/auth/grant") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		capturedAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "dg_ephemeral_tok", "expires_in": 30})
	}))
	defer srv.Close()

	tok, exp, err := mintDeepgramToken(srv.Client(), srv.URL, "dg-REAL-secret-key")
	if err != nil {
		t.Fatalf("deepgram mint: %v", err)
	}
	if tok != "dg_ephemeral_tok" || exp != 30 {
		t.Fatalf("unexpected mint result: token=%q exp=%d", tok, exp)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(capturedBody), &body); err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	for k := range body {
		if k != "ttl_seconds" {
			t.Fatalf("PRIVACY BOUNDARY: unexpected field %q in deepgram grant request: %s", k, capturedBody)
		}
	}
	if capturedAuth != "Token dg-REAL-secret-key" {
		t.Fatalf("real key should authorize the grant, got %q", capturedAuth)
	}
	if strings.Contains(tok, "dg-REAL") {
		t.Fatal("PRIVACY BOUNDARY: the real key must never be returned to the caller")
	}
}

// TestVoiceAvailable_DeepgramGatedOnVoiceCapability: Deepgram is offered for voice
// ONLY once its credential verified "voice" — never on mere key presence, and it
// must never claim stt/tts (it is voice conversation, not a karaoke STT path).
func TestVoiceAvailable_DeepgramGatedOnVoiceCapability(t *testing.T) {
	srv, store, _ := newTestServer(t)
	id, err := store.UpsertProviderCredential("deepgram", "", map[string]string{"api_key": "dg-x"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if srv.credentialHasCapability("deepgram", "voice") {
		t.Fatal("deepgram must NOT be voice-eligible before it verifies voice")
	}
	if err := store.SetCredentialCapabilities(id, []string{"voice"}); err != nil {
		t.Fatalf("caps: %v", err)
	}
	if !srv.credentialHasCapability("deepgram", "voice") {
		t.Fatal("deepgram should be voice-eligible once it verified voice")
	}
	for _, k := range []string{"stt", "tts"} {
		if srv.credentialHasCapability("deepgram", k) {
			t.Fatalf("deepgram voice-conversation key must never claim %q (not a karaoke STT path)", k)
		}
	}
}

// TestVoiceProviderRegistry_AllThreeLand proves the slot is a slot: OpenAI
// Realtime, Gemini Live and Deepgram all live in ONE registry with the same
// shape (id/label/available/mint/legible-503), and dispatch is by lookup, not a
// per-provider case. A fourth provider is one more entry, not new plumbing.
func TestVoiceProviderRegistry_AllThreeLand(t *testing.T) {
	want := map[string]string{"openai": "webrtc", "google": "gemini-live", "deepgram": "deepgram-agent"}
	if len(voiceProviderRegistry) != len(want) {
		t.Fatalf("registry has %d providers, want %d", len(voiceProviderRegistry), len(want))
	}
	for id := range want {
		vp := findVoiceProvider(id)
		if vp == nil {
			t.Fatalf("provider %q not in the slot", id)
		}
		if vp.Label == "" || vp.Available == nil || vp.Mint == nil || vp.UnavailableMsg == "" {
			t.Fatalf("provider %q is missing a required slot field: %+v", id, vp)
		}
	}
	if findVoiceProvider("nope") != nil {
		t.Fatal("unknown provider must return nil (→ 400)")
	}
}
