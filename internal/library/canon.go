// The canonical answer for "what is this work made of" — the single contract
// every surface must agree with (cross-surface consistency assert, opened
// 2026-08-09).
//
// PJ's web header said "6 AUDIO · 1 TEXT" while mobile said "11 audio
// chapters · 12 chapters" for the same book at the same moment: both were
// honest renderings of DIFFERENT private answers. This endpoint is the one
// answer. It also refuses to pretend: when the underlying data cannot be
// presented consistently (six same-voice files split across two edition
// labels), Coherent=false with the problems named — the assert goes red on
// the DATA, which is exactly the acceptance criterion ("it must go red on
// today's data").
package library

import (
	"fmt"
	"path"
	"sort"

	"github.com/pj/abookify/internal/db"
)

// CanonEdition is one narration edition of a work.
type CanonEdition struct {
	Label      string  `json:"label"` // display label ("" = unlabeled)
	Origin     string  `json:"origin"`
	Voice      string  `json:"voice,omitempty"` // TTS voice when known
	Dir        string  `json:"dir"`             // grouping key (audio file directory)
	BookIDs    []int64 `json:"book_ids"`
	Files      int     `json:"files"`
	Duration   float64 `json:"duration_secs"`
	AudioChaps int     `json:"audio_chapters"` // chapter rows across the edition's books
}

// CanonText is one text source of a work.
type CanonText struct {
	BookID   int64  `json:"book_id"`
	Kind     string `json:"kind"` // publisher | transcript | other
	Chapters int    `json:"chapters"`
	Words    int    `json:"words"`
}

// WorkCanon is GET /api/works/{id}/canon — the numbers every surface must
// render or be wrong.
type WorkCanon struct {
	WorkID   int64          `json:"work_id"`
	Editions []CanonEdition `json:"editions"`
	Texts    []CanonText    `json:"texts"`
	// Active is what a HEADER shows: the default narration edition and text
	// source, with their counts. Raw totals live in Editions/Texts.
	Active struct {
		EditionDir   string `json:"edition_dir"`
		EditionLabel string `json:"edition_label"`
		AudioFiles   int    `json:"audio_files"`
		TextBookID   int64  `json:"text_book_id"`
		TextChapters int    `json:"text_chapters"`
	} `json:"active"`
	TotalAudioFiles int `json:"total_audio_files"`
	TotalTexts      int `json:"total_texts"`
	// Coherent=false means the data itself cannot be presented consistently;
	// Problems names why. A surface-consistency assert must FAIL on an
	// incoherent work — the disagreement lives in the data, not a renderer.
	Coherent bool     `json:"coherent"`
	Problems []string `json:"problems,omitempty"`
}

// BuildWorkCanon computes the canonical description from stored data only.
func BuildWorkCanon(store *db.Store, workID int64) (*WorkCanon, error) {
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		return nil, err
	}
	c := &WorkCanon{WorkID: workID, Coherent: true}

	groups := map[string][]*db.Book{}
	var dirs []string
	for i := range w.AudioFiles {
		b := &w.AudioFiles[i]
		d := path.Dir(b.Path)
		if _, ok := groups[d]; !ok {
			dirs = append(dirs, d)
		}
		groups[d] = append(groups[d], b)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		bs := groups[d]
		ed := CanonEdition{Dir: d}
		labels := map[string]int{}
		voices := map[string]bool{}
		for _, b := range bs {
			ed.BookIDs = append(ed.BookIDs, b.ID)
			ed.Files++
			ed.Duration += b.Duration
			ed.Origin = b.Origin
			labels[b.Edition]++
			if b.Album != "" {
				voices[b.Album] = true
				ed.Voice = b.Album
			}
			n, _ := store.ChapterCount(b.ID)
			ed.AudioChaps += n
		}
		for l := range labels {
			if l != "" {
				ed.Label = l
			}
		}
		// THE label-split incoherence (PJ's two-editions-of-three): one
		// directory, one voice, MIXED edition labels. Mobile groups by label
		// and shows two editions; web resolves one. Neither can be right
		// until the data is.
		if len(labels) > 1 {
			c.Coherent = false
			c.Problems = append(c.Problems, fmt.Sprintf(
				"edition label split in %s: %d files carry %d different labels (same narration presented as multiple editions)",
				path.Base(d), ed.Files, len(labels)))
		}
		if len(voices) > 1 {
			c.Coherent = false
			c.Problems = append(c.Problems, fmt.Sprintf(
				"mixed voices in %s: one edition directory contains %d voices", path.Base(d), len(voices)))
		}
		c.Editions = append(c.Editions, ed)
		c.TotalAudioFiles += ed.Files
	}

	for i := range w.TextFiles {
		b := &w.TextFiles[i]
		kind := "other"
		switch {
		case b.Origin == "whisper_transcript" || b.Format == "transcript":
			kind = "transcript"
		case b.Origin == "publisher_epub" || b.Origin == "publisher_mobi" || b.Origin == "publisher_pdf":
			kind = "publisher"
		}
		n, _ := store.ChapterCount(b.ID)
		words := 0
		if chs, err := store.ListChapters(b.ID); err == nil {
			for _, ch := range chs {
				words += ch.WordCount
			}
		}
		c.Texts = append(c.Texts, CanonText{BookID: b.ID, Kind: kind, Chapters: n, Words: words})
		c.TotalTexts++
	}

	// Active resolution: the display text book (work-level setting, else the
	// first publisher text, else the first text), and the narration edition
	// that display resolution favors — TTS edition generated from the active
	// text when present, else the largest human edition.
	activeText := int64(0)
	for _, t := range c.Texts {
		if t.BookID == w.DisplayTextBookID {
			activeText = t.BookID
		}
	}
	if activeText == 0 {
		for _, t := range c.Texts {
			if t.Kind == "publisher" {
				activeText = t.BookID
				break
			}
		}
	}
	if activeText == 0 && len(c.Texts) > 0 {
		activeText = c.Texts[0].BookID
	}
	c.Active.TextBookID = activeText
	for _, t := range c.Texts {
		if t.BookID == activeText {
			c.Active.TextChapters = t.Chapters
		}
	}
	// Prefer the HUMAN narration when one exists (META d-sw-humandefault): the human
	// recording is THEIRS — bought, ripped, or downloaded; our TTS is something WE
	// generated on their machine. Silently defaulting to our own voice over the one
	// they own would contradict "your library, your content". Our TTS syncs perfectly
	// by construction, but that is not a reason to substitute it — the honest degrade
	// (weak human chain -> transcript + note) carries the cost of the honest choice.
	// Among the same kind, the longest. Deterministic — the point is ONE answer.
	var best *CanonEdition
	// Honour the user's explicit narration pick first (display_audio_book_id) — the
	// same sticky override as the text source. If they chose OUR TTS over their own
	// human reading, that holds; a preference we forget is a preference we override.
	if w.DisplayAudioBookID != 0 {
		for i := range c.Editions {
			for _, id := range c.Editions[i].BookIDs {
				if id == w.DisplayAudioBookID {
					best = &c.Editions[i]
				}
			}
		}
	}
	if best == nil {
		for i := range c.Editions {
			e := &c.Editions[i]
			eTTS := e.Origin == "tts_kokoro"
			bTTS := best != nil && best.Origin == "tts_kokoro"
			switch {
			case best == nil:
				best = e
			case bTTS && !eTTS: // a human edition beats our TTS — it's their content
				best = e
			case eTTS == bTTS && e.Duration > best.Duration: // same kind → longest
				best = e
			}
		}
	}
	if best != nil {
		c.Active.EditionDir = best.Dir
		c.Active.EditionLabel = best.Label
		c.Active.AudioFiles = best.Files
	}
	return c, nil
}
