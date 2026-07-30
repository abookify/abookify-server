package server

import (
	"context"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

// Gemini Live is delivered on web via a SERVER-SIDE RELAY, not the browser's own
// WebSocket. Google's ephemeral tokens do not authenticate the Live WS for our
// key (verified with a real call: access_token → 1008 "unregistered caller",
// key → 1007 "invalid key", while the RAW key works), so rather than leak the
// real key to the browser we keep it HERE and bridge frames. The user's own
// server (their hardware) sits in the path; ONLY the browser's own messages —
// setup, this turn's audio, and the reading-position-bounded search_book tool
// responses — reach Google. The relay inspects, injects, logs and stores
// NOTHING, so the egress boundary is exactly the browser's traffic PLUS the API
// key for auth (added only on the upstream dial). This keeps the key strictly
// server-side — stronger than an ephemeral token, which would be browser-visible.

const geminiLiveWSHost = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

// geminiLiveWSURL is the upstream Live WS URL. EGRESS BOUNDARY: the ONLY thing
// this server adds toward Google is the API key (for auth) — never any book or
// library text. TestGeminiRelayUpstreamURL_KeyOnly locks that.
func geminiLiveWSURL(apiKey string) string {
	return geminiLiveWSHost + "?key=" + url.QueryEscape(apiKey)
}

// handleGeminiRelay bridges the browser's Live WS to Google's Live API with the
// real key held server-side. GET /api/voice/gemini-relay (WS upgrade).
func (s *Server) handleGeminiRelay(w http.ResponseWriter, r *http.Request) {
	if !s.credentialHasCapability("google", "voice") {
		http.Error(w, "no Google voice credential", http.StatusServiceUnavailable)
		return
	}
	key := s.store.CredentialAPIKey("google")
	if key == "" {
		http.Error(w, "no Google credential", http.StatusServiceUnavailable)
		return
	}

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	client.SetReadLimit(16 << 20) // audio + tool-response frames can exceed the 32 KB default

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	upstream, _, err := websocket.Dial(ctx, geminiLiveWSURL(key), nil)
	if err != nil {
		client.Close(websocket.StatusInternalError, "upstream connect failed")
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "")
	upstream.SetReadLimit(16 << 20)

	// Two transparent pipes; the first error on either side tears both down.
	// Frames are copied byte-for-byte with their type (text JSON / binary audio)
	// — nothing is parsed, added, logged, or persisted.
	pipe := func(src, dst *websocket.Conn) {
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			if err := dst.Write(ctx, typ, data); err != nil {
				cancel()
				return
			}
		}
	}
	go pipe(client, upstream)
	pipe(upstream, client)
}
