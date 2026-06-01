package library

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
)

// castHTTP gives BookNLP plenty of time — a full novel is minutes of CPU.
var castHTTP = &http.Client{Timeout: 30 * time.Minute}

// castResponse is the booknlp service's /extract payload.
type castResponse struct {
	Characters []struct {
		Name         string   `json:"name"`
		Aliases      []string `json:"aliases"`
		Gender       string   `json:"gender"`
		MentionCount int      `json:"mention_count"`
	} `json:"characters"`
	Error string `json:"error"`
}

// CastEPUBBook returns the work's EPUB text book to extract a cast from, or
// nil if the work has none. EPUB-only by design: transcripts and PDFs aren't
// fed to BookNLP. Skips internal pipeline sources.
func CastEPUBBook(work *db.Work) *db.Book {
	for i := range work.TextFiles {
		b := &work.TextFiles[i]
		if b.Format == "epub" && b.Visibility != "internal" {
			return b
		}
	}
	return nil
}

// ExtractCast runs the booknlp service over a work's EPUB and stores the
// resulting cast. Returns the number of characters found. EXPERIMENTAL: the
// MVP cast is honest-but-imperfect (aliases sharing no tokens over-split), so
// every UI surface that shows it must carry an "experimental" badge.
func ExtractCast(store *db.Store, booknlpURL string, workID int64) (int, error) {
	if booknlpURL == "" {
		return 0, fmt.Errorf("booknlp service not configured")
	}
	work, err := store.GetWork(workID)
	if err != nil {
		return 0, err
	}
	if work == nil {
		return 0, fmt.Errorf("work %d not found", workID)
	}
	book := CastEPUBBook(work)
	if book == nil {
		return 0, fmt.Errorf("work %d has no EPUB text source", workID)
	}

	// Concatenate the book's chapter plaintext in order.
	chapters, err := store.ListChapters(book.ID)
	if err != nil {
		return 0, fmt.Errorf("list chapters: %w", err)
	}
	var sb strings.Builder
	for _, ch := range chapters {
		full, err := store.GetChapterContent(book.ID, ch.Index)
		if err != nil || full == nil || full.Content == "" {
			continue
		}
		sb.WriteString(full.Content)
		sb.WriteString("\n\n")
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return 0, fmt.Errorf("book %d has no extractable text", book.ID)
	}

	applog.Log(applog.LevelInfo, "booknlp", "", workID, "cast extraction started",
		map[string]any{"book_id": book.ID, "chars": len(text)})

	reqBody, _ := json.Marshal(map[string]string{"text": text})
	resp, err := castHTTP.Post(strings.TrimRight(booknlpURL, "/")+"/extract",
		"application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("booknlp request: %w", err)
	}
	defer resp.Body.Close()

	var cr castResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return 0, fmt.Errorf("decode booknlp response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || cr.Error != "" {
		return 0, fmt.Errorf("booknlp error: %s", cr.Error)
	}

	cast := make([]db.Character, 0, len(cr.Characters))
	for _, c := range cr.Characters {
		cast = append(cast, db.Character{
			Name:         c.Name,
			Aliases:      c.Aliases,
			Gender:       c.Gender,
			MentionCount: c.MentionCount,
		})
	}
	if err := store.ReplaceCharactersForBook(workID, book.ID, cast); err != nil {
		return 0, fmt.Errorf("store cast: %w", err)
	}

	applog.Log(applog.LevelInfo, "booknlp", "", workID, "cast extraction done",
		map[string]any{"book_id": book.ID, "characters": len(cast)})
	return len(cast), nil
}
