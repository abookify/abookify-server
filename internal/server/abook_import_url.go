package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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

// --- Sample library proxy (the first-run "try a sample" picker) ---

// sampleManifestURL is the canonical, stable manifest both the showcase page and
// this picker render from — one list, two surfaces, no drift.
const sampleManifestURL = "https://abookify.com/showcase/showcase.json"
const sampleCoverBase = "https://abookify.com/showcase/"

// SampleBook is the normalized shape the empty-library picker renders. The
// narration fields carry the quality distinction as DATA (ours vs a human
// volunteer), so each surface presents it in its own voice without guessing.
type SampleBook struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	NarrationKind string `json:"narration_kind"`  // "ours" | "human" | "neural" | "radio"
	NarrationText string `json:"narration_label"` // human-readable, from the manifest
	Voice         string `json:"voice,omitempty"`
	OurNarration  bool   `json:"our_narration"` // Kokoro — word-perfect sync by construction
	DurationSecs  int    `json:"duration_secs"` // 0 until the manifest carries it
	SizeBytes     int64  `json:"size_bytes"`
	DownloadURL   string `json:"download_url"`
	CoverURL      string `json:"cover_url"`
}

type manifestBook struct {
	Slug         string `json:"slug"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	Narration    string `json:"narration"`
	Category     string `json:"category"`
	Voice        string `json:"voice"`
	DurationSecs int    `json:"duration_secs"`
	SizeBytes    int64  `json:"size_bytes"`
	DownloadURL  string `json:"download_url"`
	Cover        string `json:"cover"`
}

var (
	samplesMu     sync.Mutex
	samplesCache  []SampleBook
	samplesExpiry time.Time
)

// GET /api/samples — the normalized sample library for the first-run picker.
// Server-side proxy of the canonical manifest: avoids a cross-origin fetch from
// the app, and reuses our trust boundary (the manifest is on our own host).
// Cached briefly so an empty-library render doesn't hammer the site.
func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	samplesMu.Lock()
	fresh := samplesCache != nil && time.Now().Before(samplesExpiry)
	cached := samplesCache
	samplesMu.Unlock()
	if fresh {
		writeJSON(w, http.StatusOK, map[string]any{"samples": cached})
		return
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(sampleManifestURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		if cached != nil { // serve stale on a transient fetch failure
			writeJSON(w, http.StatusOK, map[string]any{"samples": cached, "stale": true})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "sample library is unreachable right now"})
		return
	}
	defer resp.Body.Close()
	var doc struct {
		Books []manifestBook `json:"books"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "sample library manifest was unreadable"})
		return
	}

	out := make([]SampleBook, 0, len(doc.Books))
	for _, b := range doc.Books {
		if b.DownloadURL == "" {
			continue // not importable → don't offer it
		}
		kind := "human"
		switch b.Category {
		case "kokoro":
			kind = "ours"
		case "neural":
			kind = "neural"
		case "radio":
			kind = "radio"
		}
		cover := b.Cover
		if cover != "" && !strings.HasPrefix(cover, "http") {
			cover = sampleCoverBase + strings.TrimPrefix(cover, "/")
		}
		out = append(out, SampleBook{
			ID:            b.Slug,
			Title:         b.Title,
			Author:        b.Author,
			NarrationKind: kind,
			NarrationText: b.Narration,
			Voice:         b.Voice,
			OurNarration:  kind == "ours",
			DurationSecs:  b.DurationSecs,
			SizeBytes:     b.SizeBytes,
			DownloadURL:   b.DownloadURL,
			CoverURL:      cover,
		})
	}

	samplesMu.Lock()
	samplesCache = out
	samplesExpiry = time.Now().Add(10 * time.Minute)
	samplesMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"samples": out})
}
