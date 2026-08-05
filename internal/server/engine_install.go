package server

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pj/abookify/internal/applog"
)

// First-run install of the on-device speech engine (#57 gap). The desktop bundle
// ships the engine SOURCE + install-engine.sh but nothing invoked it — leaving a
// non-technical user with no way to turn on narration/transcripts (the product's
// whole point). This wires a real, TRIGGERED, VISIBLE install: POST starts it,
// GET reports honest byte progress (the engine dir grows toward a known total),
// and it RESUMES on re-run (build.sh caches python-build-standalone + pip wheels,
// so a drop at 80% picks up from cache, not from zero).
//
// Deliberately NOT here: running the engine after it installs. Lifecycle of the
// engine process is the Tauri shell's job (locked decision — see
// engineDeviceHintPath). The shell spawns ~/.abookify/engine/abookify-engine on
// its next launch. Making it zero-restart is a small shell change, flagged to the
// desktop lane rather than duplicating process management in the server.

const (
	engineExpectedBytesCPU  int64 = 2_100_000_000 // ~2.1 GB CPU stack (no torch+CUDA)
	engineExpectedBytesCUDA int64 = 6_300_000_000 // ~6.3 GB with torch+CUDA+cuDNN
)

type engineInstallState struct {
	mu        sync.Mutex
	running   bool
	done      bool
	failed    bool
	variant   string // "cpu" | "cuda"
	targetDir string
	logPath   string
	errMsg    string
	startedAt time.Time
}

// installEngineSrc resolves install-engine.sh: an explicit override, then the
// bundle layout (engine-src next to the server binary), then the dev tree.
func (s *Server) installEngineSrc() string {
	candidates := []string{}
	if v := os.Getenv("ABOOKIFY_ENGINE_SRC"); v != "" {
		candidates = append(candidates, filepath.Join(v, "install-engine.sh"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "engine-src", "install-engine.sh"))
	}
	candidates = append(candidates, "engine/install-engine.sh") // dev tree (cwd = engineering/server)
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// engineTargetDir is where the built engine lands — the same path the shell
// resolves on next launch (~/.abookify/engine, i.e. <data-dir>/engine).
func (s *Server) engineTargetDir() string {
	if s.DataDir == "" {
		return ""
	}
	return filepath.Join(s.DataDir, "engine")
}

// engineInstalled reports whether a usable engine launcher already exists.
func (s *Server) engineInstalled() bool {
	t := s.engineTargetDir()
	if t == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(t, "abookify-engine"))
	return err == nil && !fi.IsDir()
}

// detectEngineVariant mirrors build.sh: an explicit ABOOKIFY_ENGINE_VARIANT wins,
// else cuda iff Linux/x86_64 + an NVIDIA GPU, else cpu.
func detectEngineVariant() string {
	if v := os.Getenv("ABOOKIFY_ENGINE_VARIANT"); v == "cpu" || v == "cuda" {
		return v
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		if out, err := exec.Command("nvidia-smi", "-L").Output(); err == nil && strings.Contains(string(out), "GPU") {
			return "cuda"
		}
	}
	return "cpu"
}

// handleInstallLocalEngine (POST /api/engines/install-local) starts — or resumes —
// the on-device engine build in the background. Single-flight; returns immediately.
func (s *Server) handleInstallLocalEngine(w http.ResponseWriter, r *http.Request) {
	st := &s.engineInstall
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"state": "running", "note": "install already in progress"})
		return
	}
	if s.engineInstalled() {
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"state": "done", "note": "engine already installed"})
		return
	}
	script := s.installEngineSrc()
	target := s.engineTargetDir()
	if script == "" || target == "" {
		st.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "engine source not found — this build has no bundled installer (set ABOOKIFY_ENGINE_SRC)",
		})
		return
	}
	variant := detectEngineVariant()
	// Disk pre-check: refuse before a doomed multi-GB download rather than failing
	// with a stack trace partway. Need the engine + headroom for models later.
	need := engineExpectedBytesCPU
	if variant == "cuda" {
		need = engineExpectedBytesCUDA
	}
	if free := fsFreeBytes(target); free > 0 && free < need {
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"state": "failed",
			"error": fmt.Sprintf("not enough disk space: the %s engine needs about %d GB but only %d GB is free. Free up space and try again.",
				variant, need/1_000_000_000, free/1_000_000_000),
		})
		return
	}
	_ = os.MkdirAll(target, 0o755)
	logPath := filepath.Join(target, "install.log")
	logF, err := os.Create(logPath)
	if err != nil {
		st.mu.Unlock()
		writeServerError(w, r, err)
		return
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "ABOOKIFY_ENGINE_DIR="+target, "ABOOKIFY_ENGINE_VARIANT="+variant)
	cmd.Stdout = logF
	cmd.Stderr = logF
	st.running, st.done, st.failed = true, false, false
	st.variant, st.targetDir, st.logPath, st.errMsg = variant, target, logPath, ""
	st.startedAt = time.Now()
	st.mu.Unlock()

	applog.Infof("system", "engine install started (variant %s) -> %s", variant, target)
	if err := cmd.Start(); err != nil {
		st.mu.Lock()
		st.running, st.failed, st.errMsg = false, true, err.Error()
		st.mu.Unlock()
		logF.Close()
		writeServerError(w, r, err)
		return
	}
	go func() {
		err := cmd.Wait()
		logF.Close()
		st.mu.Lock()
		st.running = false
		if err != nil {
			st.failed, st.errMsg = true, lastLogLines(logPath, 3)
			applog.Warnf("system", "engine install FAILED: %v", err)
		} else {
			st.done = true
			applog.Infof("system", "engine install DONE -> %s (restart the app / next shell launch spawns it)", target)
		}
		st.mu.Unlock()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"state": "running", "variant": variant})
}

// handleInstallLocalEngineStatus (GET /api/engines/install-local/status) reports
// honest byte progress (dir size vs the variant's known total), the current step,
// and the terminal state so the UI shows "1.4 GB of ~2.1 GB", not a spinner.
func (s *Server) handleInstallLocalEngineStatus(w http.ResponseWriter, r *http.Request) {
	st := &s.engineInstall
	st.mu.Lock()
	running, done, failed := st.running, st.done, st.failed
	variant, target, logPath, errMsg := st.variant, st.targetDir, st.logPath, st.errMsg
	st.mu.Unlock()

	if variant == "" {
		variant = detectEngineVariant()
	}
	if target == "" {
		target = s.engineTargetDir()
	}
	total := engineExpectedBytesCPU
	if variant == "cuda" {
		total = engineExpectedBytesCUDA
	}
	installed := s.engineInstalled()
	state := "idle"
	switch {
	case running:
		state = "running"
	case failed:
		state = "failed"
	case done || installed:
		state = "done"
	case target != "" && dirSize(target) > 0:
		state = "partial" // a prior attempt left bytes → resumable
	}
	done_ := dirSize(target)
	pct := 0
	if total > 0 {
		if p := int(done_ * 100 / total); p < 100 {
			pct = p
		} else {
			pct = 99 // never claim 100% until the launcher actually exists
		}
	}
	if installed {
		pct = 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":       state,
		"variant":     variant,
		"bytes_done":  done_,
		"bytes_total": total,
		"percent":     pct,
		"step":        lastStep(logPath),
		"installed":   installed,
		"free_bytes":  fsFreeBytes(target),
		"error":       errMsg,
	})
}

// dirSize sums the byte size of every regular file under dir (our progress signal).
func dirSize(dir string) int64 {
	if dir == "" {
		return 0
	}
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// lastStep returns the most recent human-readable step marker build.sh printed
// (lines starting with "---" or "==="), so the UI can show what's happening now.
func lastStep(logPath string) string {
	if logPath == "" {
		return ""
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "---") || strings.HasPrefix(l, "===") {
			return strings.TrimLeft(l, "-= ")
		}
	}
	return ""
}

// lastLogLines returns the tail of the install log for a failure message.
func lastLogLines(logPath string, n int) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "install failed (no log)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
