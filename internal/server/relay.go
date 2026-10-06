package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/pj/abookify/internal/db"
)

const (
	settingServerID    = "server_install_id"
	settingRelayDomain = "relay_domain" // e.g. "abookify.nullbore.com"
	// settingRelayE2EDomain names the relay's end-to-end namespace (e.g.
	// "abookify.e2e.nullbore.com"). When set AND this server has a TLS
	// listener, the public URL is https://<server_id>.<e2e-domain>: the relay
	// forwards the phone's TLS to us without terminating it, and the phone
	// pins our key from the QR. Env NULLBORE_E2E_DOMAIN seeds it.
	settingRelayE2EDomain = "relay_e2e_domain"
	// settingRelayE2EPrimary ("1"/"true"; env NULLBORE_E2E_PRIMARY) makes the
	// end-to-end URL the pairing payload's primary `url`. Until the apps pin
	// the server's key they cannot talk to a self-signed listener, so the
	// primary stays the proxied URL and the end-to-end address rides along in
	// `tls_url` + `tls_spki_sha256` for apps that can. Flip once mobile ships
	// pinning.
	settingRelayE2EPrimary = "relay_e2e_primary"
)

// ServerID returns a stable UUID for this install, minting on first access.
// It's used as the nullbore tunnel slug so the public URL is stable across restarts.
func (s *Server) ServerID() string {
	id, _ := s.store.GetSetting(settingServerID)
	if id != "" {
		return id
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id = hex.EncodeToString(buf)
	_ = s.store.SetSetting(settingServerID, id)
	return id
}

// PublicURL returns the externally-reachable URL for this server.
// Precedence: ABOOKIFY_PUBLIC_URL env > relay_domain setting + server_id > request-derived.
func (s *Server) PublicURL(r *http.Request) string {
	if v := os.Getenv("ABOOKIFY_PUBLIC_URL"); v != "" {
		return v
	}
	if s.tlsPin != "" && s.relayE2EPrimary() {
		if h := s.E2EHost(); h != "" {
			return "https://" + h
		}
	}
	domain, _ := s.store.GetSetting(settingRelayDomain)
	if domain == "" {
		domain = os.Getenv("NULLBORE_BASE_DOMAIN")
	}
	if domain != "" {
		return fmt.Sprintf("https://%s.%s", s.ServerID(), domain)
	}
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	host := "localhost:7654"
	if r != nil {
		host = r.Host
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

// E2EHost is this server's hostname in the relay's end-to-end namespace:
// <server_id>-e2e.<domain>. The suffix keeps the end-to-end tunnel's name
// distinct from the proxied one (the relay refuses two tunnels of one name),
// so devices paired on the old URL keep working while they migrate.
func (s *Server) E2EHost() string {
	e2e := s.relayE2EDomain()
	if e2e == "" {
		return ""
	}
	return s.ServerID() + "-e2e." + e2e
}

// relayE2EPrimary reports whether the end-to-end URL should be the primary
// pairing address (see settingRelayE2EPrimary).
func (s *Server) relayE2EPrimary() bool {
	v, _ := s.store.GetSetting(settingRelayE2EPrimary)
	if v == "" {
		v = os.Getenv("NULLBORE_E2E_PRIMARY")
	}
	return v == "1" || strings.EqualFold(v, "true")
}

// relayE2EDomain returns the configured end-to-end relay namespace, or "".
func (s *Server) relayE2EDomain() string {
	if v, _ := s.store.GetSetting(settingRelayE2EDomain); v != "" {
		return v
	}
	return os.Getenv("NULLBORE_E2E_DOMAIN")
}

// pairingTokens holds short-lived pairing tokens. Each token authorizes one device registration.
type pairingTokens struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

var pairing = &pairingTokens{tokens: make(map[string]time.Time)}

const pairingTokenTTL = 10 * time.Minute

// Issue creates a new pairing token valid for pairingTokenTTL.
func (p *pairingTokens) Issue() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gc()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	tok := hex.EncodeToString(buf)
	p.tokens[tok] = time.Now().Add(pairingTokenTTL)
	return tok
}

// Consume validates and removes a token. Returns true if it was valid.
func (p *pairingTokens) Consume(tok string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gc()
	exp, ok := p.tokens[tok]
	if !ok || time.Now().After(exp) {
		return false
	}
	delete(p.tokens, tok)
	return true
}

func (p *pairingTokens) gc() {
	now := time.Now()
	for t, exp := range p.tokens {
		if now.After(exp) {
			delete(p.tokens, t)
		}
	}
}

// handleServerInfo returns install UUID and public URL. Used by the relay
// bootstrap script and by admin UI.
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"server_id":  s.ServerID(),
		"public_url": s.PublicURL(r),
	}
	if s.tlsPin != "" {
		info["tls_spki_sha256"] = s.tlsPin
		info["tls_url"] = s.tlsURL(r)
		info["tls_port"] = s.tlsPort
	}
	writeJSON(w, http.StatusOK, info)
}

// handleRotateServerID mints a fresh server_install_id, invalidating
// the existing tunnel slug. The caller (the settings UI) is expected
// to follow up by restarting the nullbore container with the new
// NULLBORE_TUNNELS slug — the server can't reach across to the relay
// container itself, so we return the new public_url plus a hint and
// let the user finish the rotation outside the process (#176).
//
// All previously paired mobile devices will need to re-pair because
// they hold the old hostname.
func (s *Server) handleRotateServerID(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "rng failed"})
		return
	}
	newID := hex.EncodeToString(buf)
	if err := s.store.SetSetting(settingServerID, newID); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"server_id":  newID,
		"public_url": s.PublicURL(r),
		"next_step":  "restart the relay container so it picks up the new slug",
		"command":    "cd engineering/relay && ./start.sh",
	})
}

// PairingPayload is what the QR code encodes and what the phone parses.
type PairingPayload struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// AuthToken is a 30-day login token, embedded only when auth is
	// enabled, so a scanned device is authenticated without typing the
	// password (#197). Omitted (empty) on open servers. The pair
	// endpoints are gated, so only an already-authenticated user can
	// generate a QR that carries one.
	AuthToken string `json:"auth_token,omitempty"`
	// TLSPin is base64(sha256(SPKI)) of this server's self-signed TLS key.
	// Present whenever the server has a TLS listener. The phone pins it:
	// a TLS handshake that completes against this key cannot have been
	// terminated by a relay in between. Rotating the key means re-pairing.
	TLSPin string `json:"tls_spki_sha256,omitempty"`
	// TLSURL is the https URL that reaches the TLS listener (the relay's
	// end-to-end hostname when configured, else the LAN https port). Empty
	// when TLS is off. URL stays the primary address for compatibility.
	TLSURL string `json:"tls_url,omitempty"`
}

// newPairingPayload builds the payload, minting a session-backed auth
// token when auth is enabled. The auth token is a real 30-day
// auth_sessions row (distinct from the short-lived single-use pairing
// Token, which authorizes device registration).
func (s *Server) newPairingPayload(r *http.Request) PairingPayload {
	p := PairingPayload{
		URL:   s.PublicURL(r),
		Token: pairing.Issue(),
	}
	if s.tlsPin != "" {
		p.TLSPin = s.tlsPin
		p.TLSURL = s.tlsURL(r)
	}
	if s.authEnabled() {
		if tok, err := db.NewSessionToken(); err == nil {
			user, _ := s.store.GetSetting("auth_username")
			// The paired device inherits the pairing admin's reader identity, so
			// their position/bookmarks/Q&A follow them onto the phone.
			if err := s.store.CreateAuthSession(tok, userIDFromContext(r), user, db.DefaultSessionTTL); err == nil {
				p.AuthToken = tok
			}
		}
	}
	return p
}

// tlsURL is where the TLS listener is reachable: the relay's end-to-end
// hostname when configured, else this host's TLS port.
func (s *Server) tlsURL(r *http.Request) string {
	if h := s.E2EHost(); h != "" {
		return "https://" + h
	}
	host := "localhost"
	if r != nil {
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		} else {
			host = r.Host
		}
	}
	return fmt.Sprintf("https://%s:%s", host, s.tlsPort)
}

// handlePairQR issues a fresh pairing payload and encodes it as JSON in a QR.
func (s *Server) handlePairQR(w http.ResponseWriter, r *http.Request) {
	payload := s.newPairingPayload(r)
	data, err := json.Marshal(payload)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	png, err := qrcode.Encode(string(data), qrcode.Medium, 256)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Pairing-URL", payload.URL)
	w.Write(png)
}

// handlePairPayload returns the current pairing payload as JSON (for the web UI
// to display the raw values alongside the QR).
func (s *Server) handlePairPayload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.newPairingPayload(r))
}
