package server

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// URL-import for the first-run "try a sample" funnel. The empty-library screen
// hands the server a sample .abook URL and it fetches + imports directly — one
// click, no download-and-drop puzzle. A self-hosted server sits behind the
// user's router and can reach their whole home network, so an endpoint that
// fetched an ARBITRARY url would be a server-side request forgery hole pointed
// inward. Every hop (initial + redirects) is checked against an allowlist of the
// hosts our samples actually live on; anything else fails loudly.

// abookImportDomains is the registrable-domain allowlist for URL-import: our
// site (manifest + covers) and GitHub (the .abook release assets, served from
// *.githubusercontent.com after a redirect). Matched as exact host or subdomain.
var abookImportDomains = []string{
	"abookify.com",
	"github.com",
	"githubusercontent.com",
}

func abookHostAllowed(host string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i] // strip any port
	}
	for _, d := range abookImportDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// fetchAllowlistedAbook downloads a .abook from an allowlisted host to a temp
// file and returns its path. The int is the HTTP status to surface on error.
// Redirects are followed only within the allowlist, so a crafted redirect can't
// pivot to a private address.
func fetchAllowlistedAbook(rawURL string) (path string, status int, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https") {
		return "", http.StatusBadRequest, fmt.Errorf("sample URL must be an https link")
	}
	if !abookHostAllowed(u.Hostname()) {
		return "", http.StatusForbidden, fmt.Errorf("sample URL host %q is not allowed — only Abookify's sample library can be imported by URL", u.Hostname())
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".abook") {
		return "", http.StatusBadRequest, fmt.Errorf("sample URL must point to a .abook file")
	}

	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if !abookHostAllowed(req.URL.Hostname()) {
				return fmt.Errorf("redirect to disallowed host %q", req.URL.Hostname())
			}
			return nil
		},
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return "", http.StatusBadGateway, fmt.Errorf("could not fetch the sample: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", http.StatusBadGateway, fmt.Errorf("sample download failed (HTTP %d)", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "abook-url-*.abook")
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("temp file: %v", err)
	}
	tmpPath := tmp.Name()
	// Cap the body defensively (samples are tens–hundreds of MB, not GBs).
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, 2<<30)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", http.StatusBadGateway, fmt.Errorf("saving the sample failed: %v", err)
	}
	tmp.Close()
	return tmpPath, http.StatusOK, nil
}
