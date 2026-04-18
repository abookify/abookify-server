// Fetch cover art from OpenLibrary when no embedded cover exists.
// OpenLibrary covers are free with no API key required.
//
// Search: GET https://openlibrary.org/search.json?title=X&author=Y&limit=1
// Cover:  GET https://covers.openlibrary.org/b/olid/{OLID}-L.jpg
package library

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var olClient = &http.Client{Timeout: 15 * time.Second}

// FetchCoverFromOpenLibrary searches OpenLibrary by title+author and
// downloads the cover image. Returns the saved path, or "" on failure.
// Non-destructive: skips if the work already has a cover.
func FetchCoverFromOpenLibrary(title, author, coversDir string, workID int64) string {
	coverPath := filepath.Join(coversDir, fmt.Sprintf("work-%d.jpg", workID))
	if _, err := os.Stat(coverPath); err == nil {
		return coverPath // already have one
	}

	olid := searchOpenLibrary(title, author)
	if olid == "" {
		return ""
	}

	// Fetch the large cover image.
	coverURL := fmt.Sprintf("https://covers.openlibrary.org/b/olid/%s-L.jpg", olid)
	resp, err := olClient.Get(coverURL)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	// OpenLibrary returns a 1x1 transparent pixel for missing covers.
	// Check Content-Length — real covers are > 1000 bytes.
	if resp.ContentLength > 0 && resp.ContentLength < 1000 {
		return ""
	}

	os.MkdirAll(coversDir, 0755)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap
	if err != nil || len(data) < 1000 {
		return ""
	}

	if err := os.WriteFile(coverPath, data, 0644); err != nil {
		return ""
	}

	log.Printf("cover: fetched from OpenLibrary for work %d (%s)", workID, title)
	return coverPath
}

// searchOpenLibrary returns the OLID (OpenLibrary ID) of the best matching
// edition, or "" if nothing good was found.
func searchOpenLibrary(title, author string) string {
	q := url.Values{}
	q.Set("title", strings.TrimSpace(title))
	if author != "" {
		q.Set("author", strings.TrimSpace(author))
	}
	q.Set("limit", "1")
	q.Set("fields", "key,cover_edition_key")

	searchURL := "https://openlibrary.org/search.json?" + q.Encode()
	resp, err := olClient.Get(searchURL)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Docs []struct {
			Key             string `json:"key"`
			CoverEditionKey string `json:"cover_edition_key"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Docs) == 0 {
		return ""
	}

	// Prefer cover_edition_key (points to the edition with cover art).
	olid := result.Docs[0].CoverEditionKey
	if olid == "" {
		// Fall back to the work key's last segment as OLID.
		parts := strings.Split(result.Docs[0].Key, "/")
		if len(parts) > 0 {
			olid = parts[len(parts)-1]
		}
	}
	return olid
}
