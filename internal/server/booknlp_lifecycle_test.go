package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestProbeBookNLP(t *testing.T) {
	// Any HTTP response (even 404) means the service is up.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()
	if !probeBookNLP(up.URL) {
		t.Error("probeBookNLP(reachable) = false, want true")
	}
	if probeBookNLP("http://127.0.0.1:1") { // nothing listening
		t.Error("probeBookNLP(unreachable) = true, want false")
	}
}

func TestBNComposeProjectEnvOverride(t *testing.T) {
	t.Setenv("ABOOKIFY_COMPOSE_PROJECT", "myproj")
	if got := bnComposeProject(); got != "myproj" {
		t.Errorf("bnComposeProject() = %q, want myproj", got)
	}
}

// Status is disabled by default and carries the resource estimates + a friendly
// (non-docker) message so the cast panel can render the enable flow.
func TestBNStatusDefaults(t *testing.T) {
	_, store, _ := newTestServer(t)
	m := newBNManager(&Server{store: store})
	st := m.status()
	if st.State != bnDisabled {
		t.Errorf("state = %q, want disabled", st.State)
	}
	if st.DownloadGB != booknlpDownloadGB || st.RAMGB != booknlpRAMGB {
		t.Errorf("resource estimates = %v/%v", st.DownloadGB, st.RAMGB)
	}
	if st.Message == "" || containsDocker(st.Message) {
		t.Errorf("message %q must be friendly (no docker command)", st.Message)
	}
}

func containsDocker(s string) bool {
	for i := 0; i+6 <= len(s); i++ {
		if s[i:i+6] == "docker" {
			return true
		}
	}
	return false
}

// The enable handler flips the feature flag even when it can't reach docker
// (degrades to "unavailable" with a friendly message, never a raw command).
func TestBNEnableSetsFlag(t *testing.T) {
	os.Unsetenv("ABOOKIFY_COMPOSE_PROJECT")
	_, store, _ := newTestServer(t)
	s := &Server{store: store}
	s.bn = newBNManager(s)
	// No BookNLPURL + docker likely absent in the test container → unavailable.
	st := s.bn.enable()
	if v, _ := store.GetSetting("booknlp_enabled"); v != "true" {
		t.Errorf("booknlp_enabled = %q, want true", v)
	}
	if st.State != bnUnavailable && st.State != bnStarting {
		t.Errorf("state = %q, want unavailable or starting", st.State)
	}
	if containsDocker(st.Message) {
		t.Errorf("message %q leaked a docker command", st.Message)
	}
}
