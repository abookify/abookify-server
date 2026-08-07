package server

import (
	"net/http"
	"testing"
)

func TestAbookHostAllowed(t *testing.T) {
	allow := []string{
		"abookify.com", "www.abookify.com", "abookify.com:443",
		"github.com", "objects.githubusercontent.com",
		"release-assets.githubusercontent.com", "raw.githubusercontent.com",
	}
	for _, h := range allow {
		if !abookHostAllowed(h) {
			t.Errorf("host %q should be allowed", h)
		}
	}
	// The SSRF surface: internal addresses and lookalike domains must be denied.
	deny := []string{
		"localhost", "127.0.0.1", "0.0.0.0", "10.0.0.5", "192.168.1.1",
		"169.254.169.254", "metadata.google.internal", "evil.com",
		"github.com.evil.com", "abookify.com.evil.com", "notgithub.com",
		"githubusercontent.com.attacker.net",
	}
	for _, h := range deny {
		if abookHostAllowed(h) {
			t.Errorf("host %q must be denied (SSRF surface)", h)
		}
	}
}

func TestFetchAllowlistedAbookRejects(t *testing.T) {
	cases := []struct {
		name, url string
		status    int
	}{
		{"http not https", "http://abookify.com/x.abook", http.StatusBadRequest},
		{"disallowed host", "https://evil.com/x.abook", http.StatusForbidden},
		{"internal host", "https://192.168.0.10/x.abook", http.StatusForbidden},
		{"not a .abook", "https://abookify.com/x.zip", http.StatusBadRequest},
	}
	for _, c := range cases {
		_, status, err := fetchAllowlistedAbook(c.url)
		if err == nil {
			t.Errorf("%s: expected rejection", c.name)
		}
		if status != c.status {
			t.Errorf("%s: status = %d, want %d", c.name, status, c.status)
		}
	}
}
