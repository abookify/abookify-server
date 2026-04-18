// Embedded chapter markers from audio containers (M4B QuickTime chapters,
// MP3 ID3 CHAP frames, etc.). When present, these are authoritative — they
// come from the publisher and override any narrator-pattern detection.
package library

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"

	"github.com/pj/abookify/internal/db"
)

// EmbeddedChapter is one chapter extracted from an audio container's metadata.
type EmbeddedChapter struct {
	Index    int
	Title    string
	StartSec float64
	EndSec   float64
}

// ProbeEmbeddedChapters shells out to ffprobe and returns any chapter markers
// baked into the file. Empty slice (not an error) means the file has none —
// that's the common case for LibriVox / ripped MP3s.
func ProbeEmbeddedChapters(path string) ([]EmbeddedChapter, error) {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_chapters",
		path,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	return parseFFProbeChapters(out)
}

// parseFFProbeChapters is separated from the shell-out so tests can pass a
// fixture directly without invoking ffprobe.
func parseFFProbeChapters(data []byte) ([]EmbeddedChapter, error) {
	// ffprobe emits: {"chapters":[{"id":N,"start_time":"s","end_time":"s","tags":{"title":"..."}}]}
	// start_time/end_time arrive as strings (decimal seconds).
	var raw struct {
		Chapters []struct {
			ID        int64  `json:"id"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Tags      struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode ffprobe output: %w", err)
	}

	out := make([]EmbeddedChapter, 0, len(raw.Chapters))
	for i, c := range raw.Chapters {
		start, _ := parseFloat(c.StartTime)
		end, _ := parseFloat(c.EndTime)
		title := c.Tags.Title
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		out = append(out, EmbeddedChapter{
			Index:    i,
			Title:    title,
			StartSec: start,
			EndSec:   end,
		})
	}
	return out, nil
}

// PopulateEmbeddedChapters probes one audio book and, if it has embedded
// chapter markers, writes them to the chapters table with Src="embedded".
// No-op if the file has no markers. Safe to call repeatedly — only writes
// when the book currently has no chapter rows.
func PopulateEmbeddedChapters(store *db.Store, book db.Book) (int, error) {
	count, err := store.ChapterCount(book.ID)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		return 0, nil // already populated (embedded, detected, or user-defined)
	}
	chapters, err := ProbeEmbeddedChapters(book.Path)
	if err != nil {
		return 0, err
	}
	if len(chapters) == 0 {
		return 0, nil
	}
	for _, c := range chapters {
		if err := store.InsertChapter(db.Chapter{
			BookID:     book.ID,
			Index:      c.Index,
			Title:      c.Title,
			Src:        "embedded",
			StartSec:   c.StartSec,
			EndSec:     c.EndSec,
			Confidence: 1.0, // from the container, not inferred
		}); err != nil {
			return 0, err
		}
	}
	log.Printf("embedded chapters: %d from %s", len(chapters), book.Filename)
	return len(chapters), nil
}

// HasEmbeddedChapters returns true if the book's chapter rows were sourced
// from embedded container markers. Used by the detector to skip pattern
// detection when we already have ground truth.
func HasEmbeddedChapters(store *db.Store, bookID int64) (bool, error) {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return false, err
	}
	for _, ch := range chapters {
		if ch.Src == "embedded" {
			return true, nil
		}
	}
	return false, nil
}

// parseFloat is a local helper so callers don't need strconv.
func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

// embeddedChaptersAsDetected loads chapters stored with Src="embedded" and
// returns them as DetectedChapter values, ready for splitTranscriptByChapters
// and friends. Empty result when the book has no embedded markers.
func embeddedChaptersAsDetected(store *db.Store, bookID int64) []DetectedChapter {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return nil
	}
	out := make([]DetectedChapter, 0, len(chapters))
	for _, ch := range chapters {
		if ch.Src != "embedded" {
			continue
		}
		out = append(out, DetectedChapter{
			Index:      ch.Index,
			Title:      ch.Title,
			StartSec:   ch.StartSec,
			EndSec:     ch.EndSec,
			Confidence: ch.Confidence,
		})
	}
	return out
}
