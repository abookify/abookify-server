package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pj/abookify/internal/applog"
)

// startInstalledEngine spawns the freshly-installed hermetic engine and waits for
// it to actually serve, so narration works WITHOUT the user restarting the app
// (the zero-restart last mile of the first-run install).
//
// This is the ONE place the server starts the engine — only right after a
// first-run install, in the same session. Every normal launch the Tauri shell
// still owns the engine (locked decision). No orphan / no two engines:
//   - the child is spawned with Pdeathsig=SIGTERM (Linux) so it dies if the server
//     dies, and the server SIGTERMs it on drain; the server itself dies with the
//     shell (its own Pdeathsig), so shell death → server death → engine death.
//   - the shell only spawns at launch (already past by install time), and on the
//     NEXT launch the install isn't re-triggered, so only the shell spawns it then.
//
// Readiness is PROBED, not flagged: stt_server loads the (prefetched) model at
// import, BEFORE it binds its port, so "the port answers" == "the model is loaded
// and transcription is ready". We only report ready once both ports answer.
func (s *Server) startInstalledEngine() {
	launcher := filepath.Join(s.engineTargetDir(), "abookify-engine")
	if fi, err := os.Stat(launcher); err != nil || fi.IsDir() {
		return
	}
	// Ports come from the URLs the shell handed us (or the defaults); normalise the
	// URLs so the server reaches the exact engine we're binding.
	sttPort := portOf(s.STTURL, "5200")
	ttsPort := portOf(s.TTSURL, "8880")
	s.STTURL = "http://127.0.0.1:" + sttPort
	s.TTSURL = "http://127.0.0.1:" + ttsPort

	env := append(os.Environ(),
		"ABOOKIFY_STT_PORT="+sttPort,
		"ABOOKIFY_TTS_PORT="+ttsPort,
		"ABOOKIFY_DATA_DIR="+s.DataDir,
		"ABOOKIFY_MODELS_DIR="+s.ModelsDir,
	)
	// A CPU-variant engine on a GPU host must be pinned to CPU — it has no CUDA
	// wheels, so letting launch.py auto-detect a GPU would fail.
	if v, _ := os.ReadFile(filepath.Join(s.engineTargetDir(), "VARIANT")); strings.TrimSpace(string(v)) == "cpu" {
		env = append(env, "ABOOKIFY_DEVICE=cpu")
	}

	cmd := exec.Command(launcher)
	cmd.Env = env
	cmd.SysProcAttr = engineSysProcAttr() // Pdeathsig=SIGTERM on Linux → no orphan
	if lf, err := os.Create(filepath.Join(s.engineTargetDir(), "engine.log")); err == nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	if err := cmd.Start(); err != nil {
		applog.Warnf("system", "post-install engine start failed: %v", err)
		return
	}
	s.engineInstall.mu.Lock()
	s.engineChild = cmd
	s.engineInstall.mu.Unlock()
	applog.Infof("system", "post-install engine started (pid %d) on stt:%s tts:%s — waiting for ready", cmd.Process.Pid, sttPort, ttsPort)

	// Wait for BOTH ports to answer (model loads on startup — seconds, since it's
	// prefetched — not a re-download), then rebuild the providers so the Generator
	// uses the now-live engine. GET /api/engines/status reflects reachability, so
	// the UI flips to "ready" only when it truly is.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if httpAnswers(s.STTURL) && httpAnswers(s.TTSURL) {
			s.ReloadSpeech()
			applog.Infof("system", "post-install engine READY — narration enabled without a restart")
			return
		}
		time.Sleep(2 * time.Second)
	}
	applog.Warnf("system", "post-install engine did not become ready within the timeout (it'll be there on next launch)")
}

// StopInstalledEngine tears down a server-spawned engine on drain (belt to the
// Pdeathsig suspenders), so a deliberate in-app restart leaves nothing behind.
func (s *Server) StopInstalledEngine() {
	s.engineInstall.mu.Lock()
	c := s.engineChild
	s.engineChild = nil
	s.engineInstall.mu.Unlock()
	if c != nil && c.Process != nil {
		_ = c.Process.Signal(syscall.SIGTERM)
	}
}

// portOf extracts the port from a URL, falling back to def.
func portOf(rawURL, def string) string {
	if rawURL == "" {
		return def
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		if _, p, err := net.SplitHostPort(u.Host); err == nil && p != "" {
			return p
		}
	}
	return def
}

// httpAnswers reports whether an HTTP server is serving at base — any response
// (even 404) means the flask app is up; connection-refused means it isn't.
func httpAnswers(base string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
