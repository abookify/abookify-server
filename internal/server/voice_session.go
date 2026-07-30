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
// token. EGRESS BOUNDARY: token-LIFETIME config ONLY (single use + short expiry)
// — NEVER book/library text. An empty-config token is rejected by the Live WS
// with a 1008 "unregistered caller" (confirmed with a real key), so these fields
// are required; none of them carry reader content. The model + any book grounding
// are set by the client on its Live connection. TestGeminiLiveMint_NoBookText
// locks the allowed field set.
func geminiLiveTokenConfig(now time.Time) map[string]any {
	return map[string]any{
		"uses":                 1,
		"expireTime":           now.Add(30 * time.Minute).UTC().Format(time.RFC3339),
		"newSessionExpireTime": now.Add(1 * time.Minute).UTC().Format(time.RFC3339),
	}
}

// mintGeminiLiveToken mints a Gemini Live ephemeral token (POST
// /v1alpha/auth_tokens — pinned live 2026-07-29: 200, returns {"name": token}).
// The real Google key is used ONLY here, server-side; the browser gets only the
// ephemeral token, so PJ's key never reaches the client (same rule as OpenAI).
func mintGeminiLiveToken(client *http.Client, baseURL, apiKey string) (token string, err error) {
	body, _ := json.Marshal(geminiLiveTokenConfig(time.Now()))
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

const deepgramBase = "https://api.deepgram.com"

// deepgramAgentURL is the Voice Agent WebSocket the browser connects to with the
// minted temporary token. Returned to the client so the transport isn't hardcoded
// in two places.
const deepgramAgentURL = "wss://agent.deepgram.com/v1/agent/converse"

// deepgramGrantConfig is the EXACT body sent to mint a Deepgram temporary token.
// EGRESS BOUNDARY: it carries ONLY a short TTL — NEVER any book or library text.
// The Voice Agent config + any book grounding are set by the client on its own
// connection, so nothing about the reader's library leaves at mint time.
// TestVoiceSessionOutboundBoundary_NoBookText covers all three providers.
func deepgramGrantConfig() map[string]any { return map[string]any{"ttl_seconds": 30} }

// mintDeepgramToken mints a Deepgram short-lived token (POST /v1/auth/grant) from
// the real key server-side; the browser gets ONLY the temporary token, so PJ's
// key never reaches the client (same rule as OpenAI/Gemini). The token then auths
// the Voice Agent WebSocket from the browser.
func mintDeepgramToken(client *http.Client, baseURL, apiKey string) (token string, expiresIn int64, err error) {
	body, _ := json.Marshal(deepgramGrantConfig())
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/auth/grant", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("deepgram grant failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", 0, fmt.Errorf("parse deepgram grant response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("deepgram grant response had no token")
	}
	return out.AccessToken, out.ExpiresIn, nil
}

// handleVoiceSession mints an ephemeral realtime token for the browser, per
// provider (?provider=openai default | google | deepgram). It resolves the vendor key from
// the credentials vault (OpenAI also falls back to the legacy setting), so PJ's
// already-stored key lights this up with no second paste, and returns ONLY the
// ephemeral token — never the real key, for either provider.
// ---- Voice-provider slot ----
// Speech-to-speech conversation vendors share ONE shape: an availability gate
// (key presence or a verified capability) and a token mint that returns the exact
// JSON the browser gets — an ephemeral token + its transport, NEVER the real key.
// handleVoiceSession/handleVoiceAvailable iterate this registry, so adding a
// provider is ONE entry, not a new dispatch case. OpenAI Realtime, Gemini Live
// and Deepgram all land here. The per-vendor mints + gates below are the ONLY
// provider-specific code; everything else is shared.
type voiceProvider struct {
	ID             string
	Label          string
	Available      func(s *Server) bool // gate for the picker + the session mint
	UnavailableMsg string               // legible 503 when the key/capability is absent
	Mint           func(s *Server, c *http.Client) (map[string]any, error)
}

// openAIVoiceKey resolves the OpenAI key from the vault, then the legacy inline
// setting — so an already-stored key lights up voice with no second paste.
func (s *Server) openAIVoiceKey() string {
	if k := s.store.CredentialAPIKey("openai"); k != "" {
		return k
	}
	if settings, _ := s.store.GetAllSettings(); settings != nil {
		return firstNonEmptySetting(settings, "openai_api_key")
	}
	return ""
}

// voiceProviderRegistry is the slot. A fourth provider is one more entry here.
var voiceProviderRegistry = []voiceProvider{
	{
		ID: "openai", Label: "OpenAI Realtime",
		Available:      func(s *Server) bool { return s.openAIVoiceKey() != "" },
		UnavailableMsg: "no OpenAI credential — add an OpenAI key in Settings → Keys",
		Mint: func(s *Server, c *http.Client) (map[string]any, error) {
			token, expiresAt, err := mintRealtimeToken(c, openAIRealtimeBase, s.openAIVoiceKey(), defaultRealtimeModel)
			if err != nil {
				return nil, err
			}
			return map[string]any{"token": token, "expires_at": expiresAt, "model": defaultRealtimeModel, "provider": "openai", "transport": "webrtc"}, nil
		},
	},
	{
		// Gate on the VERIFIED "voice" capability, not mere key presence: a Gemini
		// key serves Gemini Live (voice) but NOT Google Cloud TTS.
		ID: "google", Label: "Google Gemini Live",
		Available:      func(s *Server) bool { return s.credentialHasCapability("google", "voice") },
		UnavailableMsg: "this Google key hasn't verified the voice (Gemini Live) capability — add/verify a Google (Gemini) key in Settings → Keys",
		Mint: func(s *Server, c *http.Client) (map[string]any, error) {
			// Gemini Live runs through the server-side relay (see voice_relay.go):
			// Google's ephemeral tokens don't authenticate the Live WS for our key,
			// so the browser connects to OUR /api/voice/gemini-relay and the real
			// key stays server-side. No token to mint — the slot just returns the
			// connection shape (a relay flag) instead of a token.
			return map[string]any{"provider": "google", "transport": "gemini-live", "model": geminiLiveModel, "relay": true}, nil
		},
	},
	{
		ID: "deepgram", Label: "Deepgram Voice Agent",
		Available:      func(s *Server) bool { return s.credentialHasCapability("deepgram", "voice") },
		UnavailableMsg: "this Deepgram key hasn't verified the voice capability — add a Deepgram key in Settings → Keys",
		Mint: func(s *Server, c *http.Client) (map[string]any, error) {
			token, expiresIn, err := mintDeepgramToken(c, deepgramBase, s.store.CredentialAPIKey("deepgram"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"token": token, "expires_in": expiresIn, "provider": "deepgram", "transport": "deepgram-agent", "agent_url": deepgramAgentURL}, nil
		},
	},
}

func findVoiceProvider(id string) *voiceProvider {
	for i := range voiceProviderRegistry {
		if voiceProviderRegistry[i].ID == id {
			return &voiceProviderRegistry[i]
		}
	}
	return nil
}

func (s *Server) handleVoiceSession(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if provider == "" {
		provider = "openai"
	}
	vp := findVoiceProvider(provider)
	if vp == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown voice provider: " + provider})
		return
	}
	if !vp.Available(s) {
		// Legible, not silent: the user learns they haven't configured this
		// provider, not that the feature is broken.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": vp.UnavailableMsg})
		return
	}
	out, err := vp.Mint(s, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVoiceAvailable lists which voice providers can be offered today — each
// provider's own availability gate (key presence or verified capability). Cheap:
// no network, no token minted. The UI gates its voice picker on this so a user is
// never invited to tap into a guaranteed failure.
func (s *Server) handleVoiceAvailable(w http.ResponseWriter, r *http.Request) {
	providers := []string{}
	for i := range voiceProviderRegistry {
		if voiceProviderRegistry[i].Available(s) {
			providers = append(providers, voiceProviderRegistry[i].ID)
		}
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
