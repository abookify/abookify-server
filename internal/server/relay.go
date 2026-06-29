package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/pj/abookify/internal/db"
)

const (
	settingServerID    = "server_install_id"
	settingRelayDomain = "relay_domain" // e.g. "abookify.nullbore.com"
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
	writeJSON(w, http.StatusOK, map[string]string{
		"server_id":  s.ServerID(),
		"public_url": s.PublicURL(r),
	})
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
		"server_id":   newID,
		"public_url":  s.PublicURL(r),
		"next_step":   "restart the relay container so it picks up the new slug",
		"command":     "cd engineering/relay && ./start.sh",
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
	if s.authEnabled() {
		if tok, err := db.NewSessionToken(); err == nil {
			user, _ := s.store.GetSetting("auth_username")
			if err := s.store.CreateAuthSession(tok, user, db.DefaultSessionTTL); err == nil {
				p.AuthToken = tok
			}
		}
	}
	return p
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
