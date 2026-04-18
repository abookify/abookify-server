// Splitting a single-chapter transcript into per-chapter entries that align
// with detected audio chapter boundaries, so the reader pane mirrors the audio
// pane.
package library

import (
	"log"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// SplitTranscriptByChapters replaces the transcript book's flat single chapter
// with N chapter entries, one per detected audio chapter. Each transcript
// chapter gets the slice of words that fall inside its audio time range, and
// carries the same start_sec/end_sec as its audio counterpart so the UI can
// navigate bidirectionally.
//
// Returns the number of transcript chapters written (0 if nothing to do).
func SplitTranscriptByChapters(
	store *db.Store,
	transcriptBookID int64,
	words []db.SyncTimestamp,
	detected []DetectedChapter,
) (int, error) {
	if transcriptBookID == 0 || len(detected) == 0 || len(words) == 0 {
		return 0, nil
	}

	// Erase the existing flat chapter(s) before writing split ones.
	if err := store.DeleteChaptersByBook(transcriptBookID); err != nil {
		return 0, err
	}

	// Words are sorted by Start (Whisper guarantees this). We walk both lists
	// in one pass: advance a word cursor across chapter ranges.
	cursor := 0
	written := 0
	for ci, ch := range detected {
		// Last chapter absorbs everything after its start so stray trailing
		// words (outro, narrator sign-off) aren't lost.
		isLast := ci == len(detected)-1

		// Advance past any words before this chapter's start (only relevant
		// for the first chapter if there's pre-chapter intro content we want
		// to keep; here we simply drop pre-first-chapter words).
		for cursor < len(words) && words[cursor].Start < ch.StartSec {
			cursor++
		}

		// Collect words until we hit the next chapter's start (or EOF).
		start := cursor
		for cursor < len(words) {
			if !isLast && words[cursor].Start >= ch.EndSec {
				break
			}
			cursor++
		}
		slice := words[start:cursor]
		content := joinWords(slice)

		if err := store.InsertChapter(db.Chapter{
			BookID:     transcriptBookID,
			Index:      ch.Index,
			Title:      ch.Title,
			Src:        "detected",
			Content:    content,
			WordCount:  len(slice),
			StartSec:   ch.StartSec,
			EndSec:     ch.EndSec,
			Confidence: ch.Confidence,
		}); err != nil {
			return written, err
		}
		written++
	}
	log.Printf("transcript-split: wrote %d chapters on book %d", written, transcriptBookID)
	return written, nil
}

// joinWords reconstructs readable text from Whisper's token list. Whisper
// prepends a space to most words (". Hello" → " Hello"); we trim leading
// space and collapse any doubles produced by empty tokens.
func joinWords(words []db.SyncTimestamp) string {
	var b strings.Builder
	b.Grow(len(words) * 6)
	for _, w := range words {
		b.WriteString(w.Word)
	}
	return strings.TrimSpace(b.String())
}
