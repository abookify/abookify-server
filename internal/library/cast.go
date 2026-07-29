package library

import (
	"errors"
	"fmt"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
)

// ErrNoCastableText flags a foreseeable INPUT condition — the work has no EPUB
// text source, or its text isn't extracted yet — so the handler can return a
// graceful 4xx instead of a bare 500. (Distinct from a real DB/server error,
// which stays a 500.)
var ErrNoCastableText = errors.New("no castable text for work")

// castStoreLimit caps how many candidates are stored. Precision falls off with
// depth — measured on Crime and Punishment, 84% at the top 25 rows against 71%
// at 35 (see ../../distribution/cast-extraction-eval.md) — so the tail is mostly
// places, allusions and one-off capitalized words. Storing it would make the
// panel look worse than the extractor is.
const castStoreLimit = 30

// castMinMentions is the recurrence floor: a name has to come back this many
// times mid-sentence before it is a character rather than a passing mention.
const castMinMentions = 3

// ExtractCast derives a work's cast from its EPUB text and stores it.
//
// This runs the LIGHTWEIGHT extractor (ExtractCastHeuristic): pure text
// statistics, in-process, under a second per book. It replaced BookNLP, which
// cost a 6.5 GB container and minutes per book for a result that measured
// close enough not to justify either — and which, on the token-disjoint alias
// case that was the whole reason to keep it, was actually worse (it merged
// Raskolnikov's diminutive "Rodya" into a different character entirely). The
// numbers are in ../../distribution/cast-extraction-eval.md.
//
// Returns the number of characters found. Still EXPERIMENTAL, and every UI
// surface showing a cast must keep its badge: places and allusions still
// surface, and aliases that share no tokens still split into separate rows.
func ExtractCast(store *db.Store, workID int64) (int, error) {
	work, err := store.GetWork(workID)
	if err != nil {
		return 0, err
	}
	if work == nil {
		return 0, fmt.Errorf("%w: work %d not found", ErrNoCastableText, workID)
	}
	book := CastEPUBBook(work)
	if book == nil {
		return 0, fmt.Errorf("%w: work %d has no EPUB text source (cast is EPUB-only)", ErrNoCastableText, workID)
	}

	// ListChapters omits Content by design, so pull each chapter's text.
	index, err := store.ListChapters(book.ID)
	if err != nil {
		return 0, fmt.Errorf("list chapters: %w", err)
	}
	chapters := make([]db.Chapter, 0, len(index))
	for _, ch := range index {
		full, err := store.GetChapterContent(book.ID, ch.Index)
		if err != nil || full == nil || full.Content == "" {
			continue
		}
		chapters = append(chapters, *full)
	}
	if len(chapters) == 0 {
		return 0, fmt.Errorf("%w: book %d has no extractable text yet", ErrNoCastableText, book.ID)
	}

	applog.Log(applog.LevelInfo, "cast", "", workID, "cast extraction started",
		map[string]any{"book_id": book.ID, "chapters": len(chapters)})

	members := ExtractCastHeuristic(chapters, castMinMentions)

	cast := make([]db.Character, 0, castStoreLimit)
	for _, m := range members {
		if len(cast) >= castStoreLimit {
			break
		}
		// Gazetteer hits are places, not people. The list is small, so this
		// fires rarely, but when it does the candidate is almost never a name.
		if m.IsPlace {
			continue
		}
		cast = append(cast, db.Character{
			Name:         m.Name,
			Aliases:      nil, // no coreference: surface forms stay separate rows
			Gender:       "",  // not inferable from frequency statistics
			MentionCount: m.Mentions,
		})
	}
	if err := store.ReplaceCharactersForBook(workID, book.ID, cast); err != nil {
		return 0, fmt.Errorf("store cast: %w", err)
	}

	applog.Log(applog.LevelInfo, "cast", "", workID, "cast extraction done",
		map[string]any{"book_id": book.ID, "characters": len(cast)})
	return len(cast), nil
}

// CastEPUBBook returns the work's EPUB text book to extract a cast from, or
// nil if the work has none. EPUB-only by design: a transcript's spelling of a
// name is unreliable and a PDF's text layer is worse. Skips internal pipeline
// sources.
func CastEPUBBook(work *db.Work) *db.Book {
	for i := range work.TextFiles {
		b := &work.TextFiles[i]
		if b.Format == "epub" && b.Visibility != "internal" {
			return b
		}
	}
	return nil
}
