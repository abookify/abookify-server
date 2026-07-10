// Fetch cover art from OpenLibrary when no embedded cover exists.
// OpenLibrary covers are free with no API key required.
//
// Search: GET https://openlibrary.org/search.json?title=X&author=Y&limit=1
// Cover:  GET https://covers.openlibrary.org/b/olid/{OLID}-L.jpg
package library

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register decoders so image.Decode validates real files
	_ "image/jpeg" // (a truncated JPEG fails to fully decode → we reject it)
	_ "image/png"
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
	// Reject a truncated/corrupt download (OpenLibrary occasionally serves a
	// partial image that passes the size check) by fully decoding it BEFORE
	// saving — this is what stopped ~30% of covers landing half-drawn.
	if !decodeOK(data) {
		log.Printf("cover: OpenLibrary returned an undecodable image for work %d (%s) — skipped", workID, title)
		return ""
	}

	// Write to a temp file then rename so a partial write can never leave a
	// corrupt cover on disk (rename is atomic on the same filesystem).
	if err := writeFileAtomic(coverPath, data); err != nil {
		return ""
	}

	log.Printf("cover: fetched from OpenLibrary for work %d (%s)", workID, title)
	return coverPath
}

// decodeOK reports whether data is a complete, decodable image (a truncated
// JPEG/PNG fails here, which is exactly what we want to reject).
func decodeOK(data []byte) bool {
	_, _, err := image.Decode(bytes.NewReader(data))
	return err == nil
}

// writeFileAtomic writes data to a temp file in the same dir, then renames it
// into place — readers never see a partially-written file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cover-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// ValidateCoverFile fully decodes a cover file; false ⇒ missing or corrupt.
func ValidateCoverFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, _, err = image.Decode(f)
	return err == nil
}

// SweepCorruptCovers decodes every cover in coversDir and deletes any that are
// corrupt/truncated, returning counts. Deleted work covers become "missing" so
// a subsequent OpenLibrary backfill refetches them.
func SweepCorruptCovers(coversDir string) (checked, deleted int) {
	entries, err := os.ReadDir(coversDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jpg") ||
			(!strings.HasPrefix(name, "work-") && !strings.HasPrefix(name, "cover-")) {
			continue
		}
		checked++
		if !ValidateCoverFile(filepath.Join(coversDir, name)) {
			if os.Remove(filepath.Join(coversDir, name)) == nil {
				deleted++
				log.Printf("cover: removed corrupt %s", name)
			}
		}
	}
	return checked, deleted
}

// CoverCandidate is one image option for the metadata editor's cover picker.
type CoverCandidate struct {
	ThumbURL string `json:"thumb_url"` // medium, for the grid
	FullURL  string `json:"full_url"`  // large, saved when picked
	Title    string `json:"title"`
	Author   string `json:"author"`
}

// SearchOpenLibraryCovers returns several candidate covers for a title/author,
// for the Jellyfin-style picker. Uses OpenLibrary's cover IDs.
func SearchOpenLibraryCovers(title, author string, limit int) ([]CoverCandidate, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("title required")
	}
	if limit <= 0 || limit > 20 {
		limit = 12
	}
	q := url.Values{}
	q.Set("title", strings.TrimSpace(title))
	if strings.TrimSpace(author) != "" {
		q.Set("author", strings.TrimSpace(author))
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("fields", "title,author_name,cover_i")

	resp, err := olClient.Get("https://openlibrary.org/search.json?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openlibrary search: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Docs []struct {
			Title      string   `json:"title"`
			AuthorName []string `json:"author_name"`
			CoverI     int      `json:"cover_i"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	out := []CoverCandidate{}
	seen := map[int]bool{}
	for _, d := range result.Docs {
		if d.CoverI <= 0 || seen[d.CoverI] {
			continue
		}
		seen[d.CoverI] = true
		auth := ""
		if len(d.AuthorName) > 0 {
			auth = d.AuthorName[0]
		}
		out = append(out, CoverCandidate{
			ThumbURL: fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", d.CoverI),
			FullURL:  fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-L.jpg", d.CoverI),
			Title:    d.Title, Author: auth,
		})
	}
	return out, nil
}

// FetchCoverToPath downloads coverURL (must be an OpenLibrary covers URL — an
// SSRF guard, since the server fetches it), validates it's a real image, and
// writes it atomically to destPath. Used when the user PICKS a candidate.
func FetchCoverToPath(coverURL, destPath string) error {
	u, err := url.Parse(coverURL)
	if err != nil || u.Scheme != "https" || u.Host != "covers.openlibrary.org" {
		return fmt.Errorf("only covers.openlibrary.org URLs are allowed")
	}
	resp, err := olClient.Get(coverURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("fetch cover: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil || len(data) < 1000 {
		return fmt.Errorf("cover image too small or unreadable")
	}
	if !decodeOK(data) {
		return fmt.Errorf("downloaded cover is not a valid image")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return writeFileAtomic(destPath, data)
}

// SaveCoverBytes validates raw image bytes (e.g. an uploaded file) and writes
// them atomically to destPath. Used for "upload your own" in the editor.
func SaveCoverBytes(data []byte, destPath string) error {
	if len(data) < 100 || !decodeOK(data) {
		return fmt.Errorf("not a valid image file")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return writeFileAtomic(destPath, data)
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
