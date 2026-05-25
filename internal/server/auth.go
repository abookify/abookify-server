package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pj/abookify/internal/db"
)

// Optional username/password auth (#197). When a password hash is set
// the whole server is gated behind a login; when it's absent the
// server is open (its prior behavior). See SESSION_HANDOFF.md
// "AUTH CONTRACT v1".

const sessionCookieName = "abookify_session"

// authEnabled reports whether a password has been configured. The
// presence of auth_password_hash is the single source of truth — set
// it to enable, clear it to disable.
func (s *Server) authEnabled() bool {
	h, _ := s.store.GetSetting("auth_password_hash")
	return h != ""
}

// requestIsHTTPS reports whether the request reached us over TLS,
// directly or through the relay (which terminates TLS and forwards
// X-Forwarded-Proto). Used to set the cookie's Secure flag only when
// it won't break plain-HTTP localhost/LAN access.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// tokenFromRequest extracts the session token from (in order) the
// session cookie, an Authorization: Bearer header, or an
// ?access_token= query param. The cookie covers same-origin web
// (media tags + WS send it automatically); the bearer header is the
// mobile path; the query param is the fallback for media tags / WS
// that can't set headers.
func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return r.URL.Query().Get("access_token")
}

// isAuthExempt returns true for paths that must stay reachable even
// when auth is on: the static SPA shell (so the browser can load the
// page that renders the login gate) plus the discovery/login
// endpoints. Media under /samples/ lives outside /api/ but is still
// gated, so it's excluded explicitly.
func isAuthExempt(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/samples/") {
		return false
	}
	// Anything not under /api/ is a static SPA asset (shell, JS, CSS,
	// favicon). The shell carries no secrets — masked settings come
	// from the gated /api/settings — so serving it openly is safe and
	// necessary for the login screen to appear.
	if !strings.HasPrefix(p, "/api/") {
		return true
	}
	switch p {
	case "/api/health", "/api/auth/status", "/api/auth/login":
		return true
	}
	return false
}

// authMiddleware gates the server when a password is configured. It is
// the innermost middleware (accessLog → cors → auth → mux) so every
// request is still logged and CORS-decorated, and OPTIONS preflight is
// short-circuited by corsMiddleware before reaching here. Accepts a
// valid cookie, bearer token, or ?access_token= query param.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() || isAuthExempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.store.ValidateAuthSession(tokenFromRequest(r)); ok {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	})
}

// handleAuthStatus is unauthenticated so a client can discover whether
// a password is required before showing a login screen. When auth is
// disabled, authenticated is reported true (nothing is gated).
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_enabled":  false,
			"authenticated": true,
		})
		return
	}
	resp := map[string]any{"auth_enabled": true, "authenticated": false}
	if user, ok := s.store.ValidateAuthSession(tokenFromRequest(r)); ok {
		resp["authenticated"] = true
		resp["username"] = user
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuthLogin verifies credentials and mints a session token,
// returned in the body (for mobile bearer use) and as an HttpOnly
// cookie (for same-origin web). 401 on bad creds.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "auth is not enabled"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	storedUser, _ := s.store.GetSetting("auth_username")
	hash, _ := s.store.GetSetting("auth_password_hash")

	// Always run the bcrypt compare even on username mismatch so login
	// timing doesn't reveal whether the username was right.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(storedUser)) == 1
	pwOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil
	if !userOK || !pwOK {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}

	token, err := db.NewSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token mint failed"})
		return
	}
	if err := s.store.CreateAuthSession(token, storedUser, db.DefaultSessionTTL); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.setSessionCookie(w, r, token, db.DefaultSessionTTL)
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "username": storedUser})
}

// handleAuthLogout invalidates the presented token and clears the
// cookie. Only the current token is dropped — other devices (e.g. a
// phone paired via QR) keep their own tokens.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if tok := tokenFromRequest(r); tok != "" {
		_ = s.store.DeleteAuthSession(tok)
	}
	s.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(ttl / time.Second),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}
