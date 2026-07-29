package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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

// handleVoiceSession mints an ephemeral realtime token for the browser. It
// resolves the OpenAI key from the credentials vault (then the legacy setting),
// so PJ's already-stored key lights this up with no second paste, and returns
// ONLY the ephemeral token + model — never the real key.
func (s *Server) handleVoiceSession(w http.ResponseWriter, r *http.Request) {
	key := s.store.CredentialAPIKey("openai")
	if key == "" {
		if settings, _ := s.store.GetAllSettings(); settings != nil {
			key = firstNonEmptySetting(settings, "openai_api_key")
		}
	}
	if key == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no OpenAI credential — add an OpenAI key in Settings → Keys",
		})
		return
	}
	token, expiresAt, err := mintRealtimeToken(&http.Client{Timeout: 15 * time.Second}, openAIRealtimeBase, key, defaultRealtimeModel)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token, // ephemeral ek_… — safe for the browser; the real key stays here
		"expires_at": expiresAt,
		"model":      defaultRealtimeModel,
		"provider":   "openai",
	})
}

// handleVoiceAvailable reports whether realtime voice can be offered — i.e. an
// OpenAI credential resolves (vault, then the legacy setting). Cheap: no network
// call, no token minted. The UI gates its voice entry point on this so PJ is
// never invited to tap into a guaranteed failure.
func (s *Server) handleVoiceAvailable(w http.ResponseWriter, r *http.Request) {
	key := s.store.CredentialAPIKey("openai")
	if key == "" {
		if settings, _ := s.store.GetAllSettings(); settings != nil {
			key = firstNonEmptySetting(settings, "openai_api_key")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": key != "",
		"provider":  "openai",
	})
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
