package server

import (
	"encoding/json"
	"net/http"
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
	default:
		return nil
	}
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
