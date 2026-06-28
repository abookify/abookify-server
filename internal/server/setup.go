package server

import (
	"net/http"
	"os"
	"time"
)

// First-run setup + model-download hooks (distribution / packaging-plan
// "First Launch"). The desktop welcome screen reads /api/setup to decide
// whether to prompt the user to install local engines or add an API key, and
// drives the install via the /api/engines hooks. Kept in its own file — these
// are the bundle-facing contract the Tauri shell (#56) builds against; the
// shape is documented in ../handoff/server-web.md.

// engineState is one speech engine's availability.
type engineState struct {
	LocalURL  string `json:"local_url"`  // configured local-engine endpoint ("" = none)
	Reachable bool   `json:"reachable"`  // local engine answered a health probe
	// APIProvider/APIConfigured are reserved for when cloud STT/TTS is wired
	// end-to-end (#54): today only the local engine path drives TTS/STT.
	APIProvider   string `json:"api_provider,omitempty"`
	APIConfigured bool   `json:"api_configured"`
	Ready         bool   `json:"ready"` // reachable OR a usable API key (== usable now)
}

// probeHealth bounds a client Health() call so a hung engine can't stall the
// setup response. Mirrors handleHealth's probe.
func probeHealth(check func() error) bool {
	if check == nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- check() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(2 * time.Second):
		return false
	}
}

// speechEngines returns the current tts + stt engine states by probing the
// wired local clients (the same ones /api/health reports).
func (s *Server) speechEngines() (tts, stt engineState) {
	tts.LocalURL = s.TTSURL
	stt.LocalURL = s.STTURL
	if s.Generator != nil {
		if c := s.Generator.TTSClient(); c != nil {
			tts.Reachable = probeHealth(c.Health)
		}
		if c := s.Generator.STTClient(); c != nil {
			stt.Reachable = probeHealth(c.Health)
		}
	}
	// "ready" today == a reachable local engine (cloud STT/TTS not yet wired,
	// see #54). When that lands, OR-in APIConfigured here.
	tts.Ready = tts.Reachable || tts.APIConfigured
	stt.Ready = stt.Reachable || stt.APIConfigured
	return
}

// handleSetup is the First-Launch state endpoint. needs_setup is true when no
// speech engine is usable — the welcome screen then offers "install local
// engines (~1.5 GB) or add an API key". Open (no auth) + version/data paths so
// the shell can render the welcome before login. LLM (Q&A) is reported too but
// doesn't gate needs_setup — it's a separate, optional capability.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	tts, stt := s.speechEngines()
	llmProvider, llmConfigured := s.llmState()

	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.Version,
		"data_dir":   s.DataDir,
		"models_dir": s.ModelsDir,
		"speech": map[string]any{
			"tts":           tts,
			"stt":           stt,
			"any_available": tts.Ready || stt.Ready,
		},
		"llm": map[string]any{
			"configured": llmConfigured,
			"provider":   llmProvider,
		},
		// The welcome screen shows when neither speech engine is usable.
		"needs_setup": !(tts.Ready || stt.Ready),
	})
}

// llmState reports whether Q&A has a usable provider (settings or env), for
// the setup readout. Reuses the same resolution ReloadLLM does.
func (s *Server) llmState() (provider string, configured bool) {
	settings, _ := s.store.GetAllSettings()
	provider = settings["llm_provider"]
	if provider == "" {
		// env fallbacks mirror ReloadLLM
		for _, e := range []struct{ env, name string }{
			{"ANTHROPIC_API_KEY", "anthropic"}, {"OPENAI_API_KEY", "openai"},
		} {
			if os.Getenv(e.env) != "" {
				return e.name, true
			}
		}
		return "", false
	}
	// ollama needs no key; others need one in settings.
	if provider == "ollama" || settings["llm_api_key"] != "" {
		return provider, true
	}
	return provider, false
}

// handleEnginesStatus reports per-engine reachability + the models dir and the
// free space there, so the install UI can warn before a ~1.5 GB download.
func (s *Server) handleEnginesStatus(w http.ResponseWriter, r *http.Request) {
	tts, stt := s.speechEngines()
	writeJSON(w, http.StatusOK, map[string]any{
		"tts":             tts,
		"stt":             stt,
		"models_dir":      s.ModelsDir,
		"models_dir_free": fsFreeBytes(s.ModelsDir),
	})
}

// handleEnginesInstall is the "install local engines" hook the welcome screen
// calls. The Go server does not download models itself — the hermetic Python
// engine (packaging Stage 2 / #56) fetches them into models_dir. This hook
// asks each configured local engine to pre-download via `POST {url}/download`
// (the engine-side contract documented in ../handoff/server-web.md). When an
// engine doesn't expose it yet, models simply download lazily on first use, so
// we report "deferred" rather than failing. When no local engine is wired at
// all, the user's path is an API key (status "unavailable").
func (s *Server) handleEnginesInstall(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"models_dir": s.ModelsDir,
		"engines":    map[string]any{},
	}
	engines := out["engines"].(map[string]any)
	for name, url := range map[string]string{"tts": s.TTSURL, "stt": s.STTURL} {
		engines[name] = triggerEngineDownload(url)
	}
	writeJSON(w, http.StatusOK, out)
}

// triggerEngineDownload POSTs to {url}/download to ask a local engine to
// pre-fetch its model. Maps the outcome to a status the UI can show:
//   unavailable — no local engine configured (use an API key instead)
//   downloading — engine accepted the pre-download (poll GET {url}/download)
//   deferred    — engine has no /download yet; model fetches on first use
//   error       — engine reachable but the request failed
func triggerEngineDownload(url string) map[string]any {
	if url == "" {
		return map[string]any{"status": "unavailable"}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url+"/download", "application/json", nil)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return map[string]any{"status": "downloading"}
	case resp.StatusCode == http.StatusNotFound:
		return map[string]any{"status": "deferred", "note": "engine fetches its model on first use"}
	default:
		return map[string]any{"status": "error", "http": resp.StatusCode}
	}
}
