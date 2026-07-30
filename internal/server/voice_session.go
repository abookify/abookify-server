package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pj/abookify/internal/library"
)

// Realtime voice conversation — server side of the WebRTC MVP.
//
// The browser must NEVER see PJ's real API key. Instead this endpoint mints a
// short-lived EPHEMERAL token (OpenAI's `ek_…`, ~1 min TTL) from the vault key
// server-side and returns only that. The browser then does the WebRTC SDP
// exchange with OpenAI directly using the ephemeral token, so the audio path is
// browser↔OpenAI peer-to-peer and never transits this server.
//
// Pinned against the LIVE API 2026-07-29 (NOT docs — the widely-documented
// POST /v1/realtime/sessions is gone/404, and gpt-4o-realtime-preview is no
// longer in the account's model list):
//   - mint:  POST /v1/realtime/client_secrets  {"session":{"type":"realtime","model":…}}
//            → {"value":"ek_…","expires_at":…,"session":{…}}
//   - model: gpt-realtime (GA, present in the models list)
//   - the browser SDP exchange target is /v1/realtime/calls?model=… (see voice.html)

// defaultRealtimeModel is pinned to a model verified present in the account's
// /v1/models list and confirmed to mint a session — not a docs value.
const defaultRealtimeModel = "gpt-realtime"

const openAIRealtimeBase = "https://api.openai.com"

// realtimeSessionConfig is the EXACT body sent to the provider to mint a token.
// EGRESS BOUNDARY: it carries ONLY session configuration (type + model) and
// NEVER any book or library text. TestVoiceSessionOutboundBoundary_NoBookText
// enforces this so the privacy stance can't regress once the transport grows.
func realtimeSessionConfig(model string) map[string]any {
	return map[string]any{
		"session": map[string]any{
			"type":  "realtime",
			"model": model,
		},
	}
}

// mintRealtimeToken posts the session config to the provider and returns the
// ephemeral client token + expiry. baseURL is injectable for tests. The real
// apiKey is used ONLY here, server-side, in the Authorization header — it is
// never returned to the caller.
func mintRealtimeToken(client *http.Client, baseURL, apiKey, model string) (token string, expiresAt int64, err error) {
	body, _ := json.Marshal(realtimeSessionConfig(model))
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/realtime/client_secrets", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("mint failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", 0, fmt.Errorf("parse mint response: %w", err)
	}
	if out.Value == "" {
		return "", 0, fmt.Errorf("mint response had no token")
	}
	return out.Value, out.ExpiresAt, nil
}

const geminiBase = "https://generativelanguage.googleapis.com"

// geminiLiveModel is PINNED against the account's ListModels (2026-07-29): the
// only bidiGenerateContent models are the gemini-2.5-flash-native-audio-* family
// + gemini-3.x live previews; the docs-era gemini-2.0-flash-live-001 is NOT in
// the account list (same rot as gemini-1.5-flash). A rolling "-latest" alias so
// it tracks the current native-audio model instead of rotting on a dated one.
const geminiLiveModel = "gemini-2.5-flash-native-audio-latest"

// geminiLiveTokenConfig is the EXACT body sent to mint a Gemini Live ephemeral
// token. EGRESS BOUNDARY: an empty session config — NEVER book/library text. The
// model + any book grounding are set by the client on its Live connection, so
// nothing about the reader's library leaves at mint time.
// TestVoiceSessionOutboundBoundary_NoBookText covers both providers.
func geminiLiveTokenConfig() map[string]any { return map[string]any{} }

// mintGeminiLiveToken mints a Gemini Live ephemeral token (POST
// /v1alpha/auth_tokens — pinned live 2026-07-29: 200, returns {"name": token}).
// The real Google key is used ONLY here, server-side; the browser gets only the
// ephemeral token, so PJ's key never reaches the client (same rule as OpenAI).
func mintGeminiLiveToken(client *http.Client, baseURL, apiKey string) (token string, err error) {
	body, _ := json.Marshal(geminiLiveTokenConfig())
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1alpha/auth_tokens?key="+url.QueryEscape(apiKey), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini live mint failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Name string `json:"name"` // the ephemeral token id (auth_tokens/…)
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse gemini mint response: %w", err)
	}
	if out.Name == "" {
		return "", fmt.Errorf("gemini mint response had no token")
	}
	return out.Name, nil
}

// handleVoiceSession mints an ephemeral realtime token for the browser, per
// provider (?provider=openai default | google). It resolves the vendor key from
// the credentials vault (OpenAI also falls back to the legacy setting), so PJ's
// already-stored key lights this up with no second paste, and returns ONLY the
// ephemeral token — never the real key, for either provider.
func (s *Server) handleVoiceSession(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if provider == "" {
		provider = "openai"
	}
	client := &http.Client{Timeout: 15 * time.Second}
	switch provider {
	case "openai":
		key := s.store.CredentialAPIKey("openai")
		if key == "" {
			if settings, _ := s.store.GetAllSettings(); settings != nil {
				key = firstNonEmptySetting(settings, "openai_api_key")
			}
		}
		if key == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no OpenAI credential — add an OpenAI key in Settings → Keys"})
			return
		}
		token, expiresAt, err := mintRealtimeToken(client, openAIRealtimeBase, key, defaultRealtimeModel)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token": token, "expires_at": expiresAt, "model": defaultRealtimeModel,
			"provider": "openai", "transport": "webrtc",
		})
	case "google":
		// Gate on the VERIFIED "voice" capability, not mere key presence: a Gemini
		// key serves Gemini Live (voice) but NOT Google Cloud TTS, so it must not
		// imply a capability it can't serve.
		if !s.credentialHasCapability("google", "voice") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this Google key hasn't verified the voice (Gemini Live) capability"})
			return
		}
		key := s.store.CredentialAPIKey("google")
		if key == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Google credential — add a Google (Gemini) key in Settings → Keys"})
			return
		}
		token, err := mintGeminiLiveToken(client, geminiBase, key)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token": token, "model": geminiLiveModel,
			"provider": "google", "transport": "gemini-live",
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown voice provider: " + provider})
	}
}

// handleVoiceAvailable reports whether realtime voice can be offered — i.e. an
// OpenAI credential resolves (vault, then the legacy setting). Cheap: no network
// call, no token minted. The UI gates its voice entry point on this so PJ is
// never invited to tap into a guaranteed failure.
func (s *Server) handleVoiceAvailable(w http.ResponseWriter, r *http.Request) {
	openaiKey := s.store.CredentialAPIKey("openai")
	if openaiKey == "" {
		if settings, _ := s.store.GetAllSettings(); settings != nil {
			openaiKey = firstNonEmptySetting(settings, "openai_api_key")
		}
	}
	// providers lists what can serve realtime voice today. OpenAI Realtime works
	// for any OpenAI key (key presence). Google/Gemini Live is gated on the
	// VERIFIED "voice" capability — a Gemini key serves Gemini Live voice but NOT
	// Google Cloud TTS, so it can't imply a capability it doesn't have.
	providers := []string{}
	if openaiKey != "" {
		providers = append(providers, "openai")
	}
	if s.credentialHasCapability("google", "voice") {
		providers = append(providers, "google")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": len(providers) > 0,
		"providers": providers,
		"provider":  "openai", // back-compat: the default/primary provider
	})
}

// handleVoiceContext is the server side of the realtime voice book-grounding
// tool. The realtime model calls it (via a function-call relayed by the browser)
// with the user's spoken question + the reader's live position; it returns ONLY
// the reading-position-bounded passages (same bound as text Q&A), so the book
// content that reaches the model can't exceed what the reader has read. Under
// extract-only it declines grounding. The bound + decline are enforced in
// library.VoiceContext (tested).
func (s *Server) handleVoiceContext(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Query         string `json:"query"`
		ScopeMode     string `json:"scope_mode"` // "reading" (spoiler-safe, default) | "book"
		ReaderBookID  int64  `json:"reader_book_id"`
		ReaderChapter int    `json:"reader_chapter"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Query) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty query"})
		return
	}
	// Default to the spoiler-safe reading scope unless the caller opts into book.
	mode := body.ScopeMode
	if mode == "" {
		mode = "reading"
	}
	scope := library.ResolveSessionScope(mode, body.ReaderBookID, body.ReaderChapter, library.QueryScope{})
	res, err := library.VoiceContext(s.store, s.RAG(), workID, body.Query, scope, s.extractOnlyEnabled())
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// firstNonEmptySetting returns the first non-empty value among the given keys.
func firstNonEmptySetting(settings map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := settings[k]; v != "" {
			return v
		}
	}
	return ""
}
