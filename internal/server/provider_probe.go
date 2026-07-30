package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// probeCredentialFn verifies which feature kinds a saved credential can actually
// serve. A package var so tests can stub it (the real one makes a network call).
var probeCredentialFn = probeCapabilities

// probeCapabilities asks the vendor which kinds the key really reaches — never
// assumed. This is the constraint PJ named: a Gemini key covers Gemini LLM/voice
// but NOT Google Cloud TTS, so we mark only what we can prove. Returns the
// VERIFIED subset of the vendor's declared kinds; best-effort (a kind it can't
// confirm is omitted), and vendors we don't yet integrate return nil — the key
// is stored but lights up no feature until a capability is proven.
func probeCapabilities(desc ProviderDescriptor, fields map[string]string) []string {
	switch desc.ID {
	case "openai":
		return probeOpenAI(fields["api_key"], "https://api.openai.com")
	case "anthropic":
		return probeAnthropic(fields["api_key"], "https://api.anthropic.com")
	case "google":
		return probeGoogle(fields["api_key"], "https://generativelanguage.googleapis.com")
	case "deepgram":
		return probeDeepgram(fields["api_key"], "https://api.deepgram.com")
	default:
		return nil
	}
}

// probeDeepgram verifies the key against Deepgram's project list. Deepgram is a
// VOICE-conversation vendor here (its Voice Agent API), NOT a karaoke STT path
// (that distinction is settled), so a working key verifies "voice" only.
func probeDeepgram(apiKey, baseURL string) []string {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/projects", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return []string{"voice"}
}

// reprobeEmptyCredentials re-runs the capability probe for any stored credential
// with empty capabilities — migrated keys (the migration doesn't probe) and keys
// saved before a probe covered their vendor. Best-effort and background; only
// touches empty ones so it doesn't re-hit vendors every boot once populated. No
// key material is logged.
func (s *Server) reprobeEmptyCredentials() {
	creds, err := s.store.ListCredentials()
	if err != nil {
		return
	}
	for _, c := range creds {
		if len(c.Capabilities) > 0 {
			continue
		}
		desc, ok := providerDescriptor(c.Provider)
		if !ok {
			continue
		}
		if caps := probeCredentialFn(desc, c.Fields); len(caps) > 0 {
			_ = s.store.SetCredentialCapabilities(c.ID, caps)
		}
	}
}

// probeAnthropic verifies the key against Anthropic's model list. Anthropic is
// an LLM vendor only, so a working key verifies "llm".
func probeAnthropic(apiKey, baseURL string) []string {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return []string{"llm"}
}

// probeGoogle verifies the key against the Gemini API model list. A working
// Gemini key serves Gemini LLM + Gemini Live (voice) — the SAME key. It does
// NOT verify "tts": Google Cloud Text-to-Speech (WaveNet/Neural2) is a separate
// GCP product requiring project enablement that a Gemini key does not reach, so
// we never claim tts from it (PJ's exact caveat).
func probeGoogle(apiKey, baseURL string) []string {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1beta/models?key="+url.QueryEscape(apiKey), nil)
	if err != nil {
		return nil
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return []string{"llm", "voice"}
}

// probeOpenAI checks the key against OpenAI's model list and returns which kinds
// the account can actually serve (llm/stt/tts), derived from the models present
// rather than assumed. A bad key or no access verifies nothing.
func probeOpenAI(apiKey, baseURL string) []string {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	var llmOK, sttOK, ttsOK bool
	for _, m := range body.Data {
		id := strings.ToLower(m.ID)
		if strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "o1") || strings.HasPrefix(id, "o3") || strings.HasPrefix(id, "chatgpt") {
			llmOK = true
		}
		if strings.Contains(id, "whisper") || strings.Contains(id, "transcribe") {
			sttOK = true
		}
		if strings.Contains(id, "tts") {
			ttsOK = true
		}
	}
	caps := []string{}
	if llmOK {
		caps = append(caps, "llm")
	}
	if sttOK {
		caps = append(caps, "stt")
	}
	if ttsOK {
		caps = append(caps, "tts")
	}
	return caps
}
