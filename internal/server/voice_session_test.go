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
