package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pj/abookify/internal/db"
)

// A desktop build (launch id set) with no password: the LAN lock applies.
func lockedServer(t *testing.T) (*Server, *db.Store) {
	t.Helper()
	srv, store, _ := newTestServer(t)
	srv.LaunchID = "lan-lock-test"
	return srv, store
}

// through sends a request through the full middleware chain (access log →
// cors → auth → mux), as a socket would, from the given remote address.
func through(srv *Server, method, path, remote, bearer string, body []byte) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = remote
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, r)
	return w
}

const (
	lan      = "192.168.1.50:51234"
	loopback = "127.0.0.1:51234"
)

func TestLanLockRefusesAStrangerOnTheWiFi(t *testing.T) {
	srv, _ := lockedServer(t)
	w := through(srv, "GET", "/api/works", lan, "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("LAN request without a token: got %d, want 401", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["server"] != "abookify" || body["lan_locked"] != true {
		t.Fatalf("401 body must name the server and the lock so a client can say 'pair by QR': %s", w.Body.String())
	}
}

func TestLanLockLeavesLoopbackFree(t *testing.T) {
	srv, _ := lockedServer(t)
	if w := through(srv, "GET", "/api/works", loopback, "", nil); w.Code != http.StatusOK {
		t.Fatalf("the shell's own window (loopback) must stay free: got %d", w.Code)
	}
}

func TestLanLockPairedPhonePasses(t *testing.T) {
	srv, store := lockedServer(t)
	// The pairing QR is rendered for the shell's window: loopback, no token.
	w := through(srv, "GET", "/api/pair-payload", loopback, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pair-payload from loopback: %d", w.Code)
	}
	var p PairingPayload
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.AuthToken == "" {
		t.Fatal("a locked desktop must always issue an auth token in the pairing payload")
	}
	if w := through(srv, "GET", "/api/works", lan, p.AuthToken, nil); w.Code != http.StatusOK {
		t.Fatalf("paired phone (bearer from the QR) on the LAN: got %d, want 200", w.Code)
	}
	// It's a device credential, not a 30-day login: a phone paired today
	// must still open the library next year (there's no password to log
	// back in with).
	exp, err := store.AuthSessionExpiresAt(p.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(time.Now().Add(365 * 24 * time.Hour)) {
		t.Fatalf("pairing token expires %v — too soon for a passwordless desktop", exp)
	}
}

func TestLanLockRegistrationStaysReachable(t *testing.T) {
	srv, _ := lockedServer(t)
	w := through(srv, "GET", "/api/pair-payload", loopback, "", nil)
	var p PairingPayload
	json.Unmarshal(w.Body.Bytes(), &p)
	// The phone registers with the one-time pairing token and NO session —
	// that endpoint guards itself with the token.
	body, _ := json.Marshal(map[string]string{"name": "phone", "platform": "mobile", "token": p.Token})
	w = through(srv, "POST", "/api/devices/register", lan, "", body)
	if w.Code == http.StatusUnauthorized && strings.Contains(w.Body.String(), "authentication required") {
		t.Fatalf("the lock must not block device registration: %d %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("register with a fresh pairing token: %d %s", w.Code, w.Body.String())
	}
	// A bad pairing token is still refused — by the handler, not the lock.
	body, _ = json.Marshal(map[string]string{"name": "phone", "platform": "mobile", "token": "nope"})
	if w := through(srv, "POST", "/api/devices/register", lan, "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad pairing token: %d", w.Code)
	}
}

func TestLanLockExemptEndpointsStayOpen(t *testing.T) {
	srv, _ := lockedServer(t)
	// (/api/health and /api/info are exempt too, but their handlers probe
	// the STT engine, which the test fixture doesn't wire.)
	for _, p := range []string{"/api/ready", "/api/auth/status"} {
		if w := through(srv, "GET", p, lan, "", nil); w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s from the LAN: %d (liveness/version must stay open for the apps' probes)", p, w.Code)
		}
	}
}

func TestLanLockTreatsRelayTrafficAsRemote(t *testing.T) {
	srv, _ := lockedServer(t)
	// A relay client on the same machine delivers internet traffic from
	// loopback. The proxied tunnel marks it X-Forwarded-Proto: https; that
	// must not count as "the shell's own window".
	r := httptest.NewRequest("GET", "/api/works", nil)
	r.RemoteAddr = loopback
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("relayed request without a token: got %d, want 401", w.Code)
	}
}

func TestLanLockDoesNotApplyToDockerOrWhenAPasswordIsSet(t *testing.T) {
	srv, store, _ := newTestServer(t) // no launch id: Docker / bare server
	if w := through(srv, "GET", "/api/works", lan, "", nil); w.Code != http.StatusOK {
		t.Fatalf("unmanaged server with auth off must be unchanged: %d", w.Code)
	}
	// Password set: the ordinary auth path, loopback included.
	srv.LaunchID = "lan-lock-test"
	store.SetSetting("auth_password_hash", "x")
	if w := through(srv, "GET", "/api/works", loopback, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("with a password, loopback without a session must be 401 as before: %d", w.Code)
	}
}

func TestServerInfoReportsLanLock(t *testing.T) {
	srv, _ := lockedServer(t)
	w := through(srv, "GET", "/api/server-info", loopback, "", nil)
	var info map[string]any
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["lan_locked"] != true {
		t.Fatalf("server-info must report lan_locked true on a passwordless desktop: %s", w.Body.String())
	}
	srv.LaunchID = ""
	w = through(srv, "GET", "/api/server-info", loopback, "", nil)
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["lan_locked"] != false {
		t.Fatalf("unmanaged server: lan_locked must be false: %s", w.Body.String())
	}
}
