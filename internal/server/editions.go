package server

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// Editions: a work's audio + text sources GROUPED by edition (a multi-file
// narrator recording is ONE edition, not N files), each tagged with a friendly
// provenance label + whether it's the current display default. Server-owned so
// web and mobile render the same contract (mobile's book-details provenance
// display consumes this).

// EditionView is one grouped edition of a work.
type EditionView struct {
	Key          string  `json:"key"`             // stable group key
	Media        string  `json:"media"`           // "audio" | "text"
	Label        string  `json:"label"`           // display name (edition/album/origin)
	Edition      string  `json:"edition"`         // raw edition label ("" = default), for relabel
	Provenance   string  `json:"provenance"`      // friendly source label
	ProvKind     string  `json:"prov_kind"`       // human | tts | transcript | publisher | personal
	BookIDs      []int64 `json:"book_ids"`        // every file in this edition
	FileCount    int     `json:"file_count"`
	Format       string  `json:"format,omitempty"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	ChapterCount int     `json:"chapter_count,omitempty"`
	IsDefault    bool    `json:"is_default"` // resolved display default for its media
}

// provenanceFor maps a book origin to a human label + coarse kind. Kept
// server-side so web + mobile show identical provenance.
func provenanceFor(origin string) (label, kind string) {
	switch origin {
	case "author_recording":
		return "Author recording", "human"
	case "narrator_recording":
		return "Narrator recording", "human"
	case "librivox":
		return "LibriVox (human)", "human"
	case "tts_kokoro":
		return "Kokoro TTS (Abookify)", "tts"
	case "tts_preprocessed":
		return "TTS source text", "tts"
	case "whisper_transcript":
		return "Transcript (speech-to-text)", "transcript"
	case "publisher_epub":
		return "Publisher EPUB", "publisher"
	case "publisher_mobi":
		return "Publisher MOBI", "publisher"
	case "publisher_pdf":
		return "Publisher PDF", "publisher"
	case "user_upload", "":
		return "Your import", "personal"
	default:
		return strings.Title(strings.ReplaceAll(origin, "_", " ")), "personal"
	}
}

// editionKey groups a book into its edition: an explicit edition label wins,
// then its album, then the parent directory (so the N files of one narrator
// recording collapse together), then origin. Mirrors the web editionKeyOf.
func editionKey(b db.Book) string {
	if e := strings.TrimSpace(b.Edition); e != "" {
		return "ed:" + e
	}
	if a := strings.TrimSpace(b.Album); a != "" {
		return "al:" + a
	}
	if b.Path != "" {
		if dir := filepath.Dir(b.Path); dir != "" && dir != "." && dir != "/" {
			return "dir:" + dir
		}
	}
	if b.Origin != "" {
		return "origin:" + b.Origin
	}
	return "book:" + strconv.FormatInt(b.ID, 10)
}

// groupEditions clusters a media list into ordered editions, tagging the one
// containing defaultID as the default.
func groupEditions(books []db.Book, media string, defaultID int64) []EditionView {
	order := []string{}
	byKey := map[string][]db.Book{}
	for _, b := range books {
		if b.Visibility == "internal" {
			continue // pipeline intermediates aren't user-facing editions
		}
		k := editionKey(b)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], b)
	}
	out := make([]EditionView, 0, len(order))
	for _, k := range order {
		grp := byKey[k]
		b := grp[0]
		prov, kind := provenanceFor(b.Origin)
		label := strings.TrimSpace(b.Edition)
		if label == "" {
			label = strings.TrimSpace(b.Album)
		}
		if label == "" {
			label = prov
		}
		ev := EditionView{
			Key: k, Media: media, Label: label, Edition: b.Edition,
			Provenance: prov, ProvKind: kind, Format: b.Format,
		}
		for _, x := range grp {
			ev.BookIDs = append(ev.BookIDs, x.ID)
			ev.DurationSecs += x.Duration
			ev.ChapterCount += x.ChapterCount
			if x.ID == defaultID {
				ev.IsDefault = true
			}
		}
		ev.FileCount = len(grp)
		out = append(out, ev)
	}
	return out
}

// handleListEditions returns a work's audio + text editions with provenance +
// default flags. GET /api/works/{id}/editions.
func (s *Server) handleListEditions(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	var audioDefault, textDefault int64
	if b := library.ResolveDisplayAudio(work); b != nil {
		audioDefault = b.ID
	}
	if b := library.ResolveDisplayText(work); b != nil {
		textDefault = b.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"work_id": workID,
		"audio":   groupEditions(work.AudioFiles, "audio", audioDefault),
		"text":    groupEditions(work.TextFiles, "text", textDefault),
	})
}
