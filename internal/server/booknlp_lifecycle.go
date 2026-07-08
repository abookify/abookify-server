package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pj/abookify/internal/applog"
)

// BookNLP lifecycle — treat the cast-of-characters engine like the #56 speech
// engines instead of making an end user run docker. The cast panel offers an
// in-UI "enable" that STARTS the service (containerized: the server drives the
// booknlp compose profile through the mounted docker socket); an idle timer
// STOPS it to reclaim RAM. No docker command is ever shown to the user.
//
// Security note: the server reaches docker via the mounted socket (host-root
// equivalent) and is exposed over the relay tunnel — so the enable/disable
// hooks stay behind the normal /api auth gate, and only ever touch the single
// `booknlp` compose service.

const (
	booknlpDownloadGB  = 6.5              // image size, surfaced in the enable copy
	booknlpRAMGB       = 7                // approx resident RAM when loaded
	booknlpIdleTimeout = 20 * time.Minute // auto-stop after this much inactivity
	booknlpStartWait   = 5 * time.Minute  // bound the compose up + health wait
	booknlpService     = "booknlp"
)

type bnState string

const (
	bnDisabled    bnState = "disabled"    // feature flag off (default)
	bnStopped     bnState = "stopped"     // enabled but the service isn't running
	bnStarting    bnState = "starting"    // start issued; pulling/booting/health-waiting
	bnReady       bnState = "ready"       // running + reachable
	bnStopping    bnState = "stopping"    //
	bnError       bnState = "error"       // last start/stop failed
	bnUnavailable bnState = "unavailable" // can't auto-start here (no docker access)
)

type bnManager struct {
	s  *Server
	mu sync.Mutex

	state        bnState
	lastErr      string
	lastActivity time.Time
	idleTimer    *time.Timer
}

func newBNManager(s *Server) *bnManager {
	return &bnManager{s: s, state: bnDisabled}
}

// bnStatus is the GET /api/booknlp/status payload the cast panel renders from.
type bnStatus struct {
	State        bnState `json:"state"`
	Enabled      bool    `json:"enabled"`       // feature flag
	Reachable    bool    `json:"reachable"`     // service answered a probe
	CanAutostart bool    `json:"can_autostart"` // server can drive docker here
	DownloadGB   float64 `json:"download_gb"`
	RAMGB        float64 `json:"ram_gb"`
	IdleStopMin  int     `json:"idle_stop_min"`
	Message      string  `json:"message"`
	Error        string  `json:"error,omitempty"`
}

func (m *bnManager) status() bnStatus {
	m.mu.Lock()
	state, lastErr := m.state, m.lastErr
	m.mu.Unlock()

	settings, _ := m.s.store.GetAllSettings()
	enabled := settings["booknlp_enabled"] == "true"
	reachable := m.s.BookNLPURL != "" && probeBookNLP(m.s.BookNLPURL)
	canAuto := m.s.BookNLPURL != "" && dockerAvailable()

	// Reconcile: if the service is reachable, we're ready regardless of what a
	// prior transition recorded (survives restarts / external starts).
	if reachable && state != bnStopping {
		state = bnReady
	} else if !enabled {
		state = bnDisabled
	} else if state == bnReady && !reachable {
		state = bnStopped
	}
	if enabled && !canAuto && !reachable {
		state = bnUnavailable
	}

	msg := ""
	switch state {
	case bnDisabled:
		msg = fmt.Sprintf("Cast extraction is off. Enabling downloads the engine (~%.1f GB) and uses ~%d GB RAM while active.", booknlpDownloadGB, booknlpRAMGB)
	case bnStarting:
		msg = "Starting the cast engine — the first run downloads it (~6.5 GB), so this can take a few minutes."
	case bnStopped:
		msg = "Cast engine is enabled but stopped. It starts on demand and stops itself after a while idle."
	case bnReady:
		msg = fmt.Sprintf("Cast engine is running. It stops itself after %d minutes idle to free memory.", int(booknlpIdleTimeout.Minutes()))
	case bnUnavailable:
		msg = "Cast extraction can't be started automatically on this server. Ask whoever runs the server to enable the experimental BookNLP engine."
	case bnError:
		msg = "The cast engine failed to start. You can try again."
	}
	return bnStatus{
		State: state, Enabled: enabled, Reachable: reachable, CanAutostart: canAuto,
		DownloadGB: booknlpDownloadGB, RAMGB: booknlpRAMGB,
		IdleStopMin: int(booknlpIdleTimeout.Minutes()), Message: msg, Error: lastErr,
	}
}

func (m *bnManager) setState(st bnState, errMsg string) {
	m.mu.Lock()
	m.state, m.lastErr = st, errMsg
	m.mu.Unlock()
}

// enable turns the feature flag on and starts the service (async). Returns the
// state immediately so the UI can poll.
func (m *bnManager) enable() bnStatus {
	m.s.store.SetSetting("booknlp_enabled", "true")
	if m.s.BookNLPURL != "" && probeBookNLP(m.s.BookNLPURL) {
		m.setState(bnReady, "")
		m.touch()
		return m.status()
	}
	if !dockerAvailable() {
		m.setState(bnUnavailable, "")
		return m.status()
	}
	m.setState(bnStarting, "")
	go func() {
		if err := m.s.bnDockerUp(); err != nil {
			applog.Log(applog.LevelError, "booknlp", "", 0, "cast engine failed to start", map[string]any{"error": err.Error()})
			m.setState(bnError, "start failed")
			return
		}
		// Wait for the service to answer.
		deadline := time.Now().Add(booknlpStartWait)
		for time.Now().Before(deadline) {
			if probeBookNLP(m.s.BookNLPURL) {
				m.setState(bnReady, "")
				m.touch()
				applog.Info("booknlp", "cast engine started + reachable")
				return
			}
			time.Sleep(3 * time.Second)
		}
		m.setState(bnError, "engine did not become reachable in time")
	}()
	return m.status()
}

// disable stops the service and clears the flag.
func (m *bnManager) disable() bnStatus {
	m.s.store.SetSetting("booknlp_enabled", "false")
	m.cancelIdle()
	if dockerAvailable() {
		m.setState(bnStopping, "")
		go func() {
			if err := m.s.bnDockerDown(); err != nil {
				applog.Log(applog.LevelWarn, "booknlp", "", 0, "cast engine stop failed", map[string]any{"error": err.Error()})
			}
			m.setState(bnDisabled, "")
		}()
	} else {
		m.setState(bnDisabled, "")
	}
	return m.status()
}

// touch records cast activity and (re)arms the idle auto-stop.
func (m *bnManager) touch() {
	m.mu.Lock()
	m.lastActivity = time.Now()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(booknlpIdleTimeout, m.idleStop)
	m.mu.Unlock()
}

func (m *bnManager) cancelIdle() {
	m.mu.Lock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	m.mu.Unlock()
}

// idleStop stops the container after inactivity but LEAVES the feature flag on,
// so the next extraction can transparently start it again.
func (m *bnManager) idleStop() {
	if !dockerAvailable() {
		return
	}
	applog.Info("booknlp", fmt.Sprintf("cast engine idle for %d min — stopping to reclaim RAM", int(booknlpIdleTimeout.Minutes())))
	m.setState(bnStopping, "")
	if err := m.s.bnDockerDown(); err != nil {
		applog.Log(applog.LevelWarn, "booknlp", "", 0, "idle stop failed", map[string]any{"error": err.Error()})
	}
	m.setState(bnStopped, "")
}

// --- docker control (the containerized controller) --------------------------

func dockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// `docker ps` needs the daemon socket, so it doubles as a capability probe.
	return exec.CommandContext(ctx, "docker", "ps").Run() == nil
}

func probeBookNLP(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	for _, u := range []string{url + "/health", url} {
		resp, err := c.Get(u)
		if err == nil {
			resp.Body.Close()
			return true // any HTTP response ⇒ the service is up
		}
	}
	return false
}

// bnComposeDir is the directory holding docker-compose.yml inside the server
// container (the repo is bind-mounted at /app in the dev/compose setup).
func bnComposeDir() string {
	for _, d := range []string{"/app", "."} {
		if _, err := os.Stat(d + "/docker-compose.yml"); err == nil {
			return d
		}
	}
	return "."
}

// bnComposeProject matches the project of the ALREADY-running stack so the
// server controls the same `booknlp` service (not a fresh project). Derived
// from the server container's own compose label; overridable via env.
func bnComposeProject() string {
	if p := strings.TrimSpace(os.Getenv("ABOOKIFY_COMPOSE_PROJECT")); p != "" {
		return p
	}
	host, _ := os.Hostname()
	if host != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format",
			`{{index .Config.Labels "com.docker.compose.project"}}`, host).Output()
		if err == nil {
			if p := strings.TrimSpace(string(out)); p != "" {
				return p
			}
		}
	}
	return "server" // repo dir basename — the default project name
}

func (s *Server) bnCompose(ctx context.Context, args ...string) error {
	full := append([]string{"compose", "--profile", booknlpService}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = bnComposeDir()
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME="+bnComposeProject())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Server) bnDockerUp() error {
	ctx, cancel := context.WithTimeout(context.Background(), booknlpStartWait)
	defer cancel()
	return s.bnCompose(ctx, "up", "-d", booknlpService)
}

func (s *Server) bnDockerDown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return s.bnCompose(ctx, "stop", booknlpService)
}
