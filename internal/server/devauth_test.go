package server

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// The dev bypass must never work on a request that came through the relay or
// the TLS listener — a persisted token on a later-exposed instance is exactly
// the shape of a backdoor.
func TestDevAuthRefusedOverHTTPS(t *testing.T) {
	s := &Server{DevAuthToken: "dev-token"}
	plain := httptest.NewRequest("GET", "/api/works", nil)
	plain.Header.Set("Authorization", "Bearer dev-token")
	if !s.devAuthOK(plain) {
		t.Fatal("plain-HTTP LAN request with the right token must pass")
	}
	fwd := httptest.NewRequest("GET", "/api/works", nil)
	fwd.Header.Set("Authorization", "Bearer dev-token")
	fwd.Header.Set("X-Forwarded-Proto", "https")
	if s.devAuthOK(fwd) {
		t.Fatal("relay-forwarded (X-Forwarded-Proto https) request must be refused")
	}
	direct := httptest.NewRequest("GET", "/api/works", nil)
	direct.Header.Set("Authorization", "Bearer dev-token")
	direct.TLS = &tls.ConnectionState{}
	if s.devAuthOK(direct) {
		t.Fatal("request on the TLS listener must be refused")
	}
}
