// Paragraph anchors in ebook chapters. Each chapter's content is split into
// paragraph records with word-position offsets, so alignments (#86) can
// target a specific paragraph rather than a whole chapter.
//
// Word positions are chapter-local — paragraph_idx 0 starts at word 0 of
// the chapter, regardless of which chapter we're in. Alignments compose
// the chapter_idx + word_start to get a book-global position when needed.
package library

import (
	"log"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// sentenceBoundaryRe matches end-of-sentence punctuation followed by a space
// and a capital letter — the heuristic we use to break transcripts (which
// arrive as one long run without newlines) into paragraph-like units.
var sentenceBoundaryRe = regexp.MustCompile(`([.!?])\s+([A-Z])`)

// paragraphTargetWords is the soft target for transcript paragraph size when
// synthesizing breaks from sentences. Transcripts tend to have many short
// sentences; grouping ~60 words per paragraph gives decent UI rhythm and
// alignment granularity without being overly chatty.
const paragraphTargetWords = 60

// SplitIntoParagraphs breaks a chapter's content into paragraph records.
// Primary: one paragraph per non-empty line (EPUB extraction converts
// block-level tags to newlines). Fallback for newline-free transcripts:
// split on sentence boundaries and group into ~60-word paragraphs.
//
// Returns paragraphs with chapter-local word offsets.
func SplitIntoParagraphs(bookID int64, chapterIdx int, content string) []db.Paragraph {
	if content == "" {
		return nil
	}

	// Primary: line-based split (EPUB, preprocessed text).
	lines := strings.Split(content, "\n")
	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}

	// Fallback: if the content is effectively one line but has real length,
	// synthesize paragraph breaks from sentence boundaries. Transcripts and
	// plain-text imports fall into this case.
	if nonEmpty <= 1 && len(strings.Fields(content)) > paragraphTargetWords {
		return splitByWordTarget(bookID, chapterIdx, content)
	}

	paras := make([]db.Paragraph, 0, nonEmpty)
	wordCursor := 0
	paraIdx := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		paras = append(paras, db.Paragraph{
			BookID:       bookID,
			ChapterIdx:   chapterIdx,
			ParagraphIdx: paraIdx,
			WordStart:    wordCursor,
			WordEnd:      wordCursor + len(words),
			Text:         line,
		})
		wordCursor += len(words)
		paraIdx++
	}
	return paras
}

// splitByWordTarget accumulates sentences until a paragraph reaches ~60 words,
// then emits. Last paragraph takes whatever remains.
func splitByWordTarget(bookID int64, chapterIdx int, content string) []db.Paragraph {
	// Inject newlines at sentence boundaries so we can walk linearly.
	marked := sentenceBoundaryRe.ReplaceAllString(content, "$1\n$2")
	sentences := strings.Split(marked, "\n")

	var paras []db.Paragraph
	var buf strings.Builder
	bufWords := 0
	wordCursor := 0
	paraIdx := 0
	flush := func() {
		text := strings.TrimSpace(buf.String())
		if text == "" {
			return
		}
		words := strings.Fields(text)
		paras = append(paras, db.Paragraph{
			BookID:       bookID,
			ChapterIdx:   chapterIdx,
			ParagraphIdx: paraIdx,
			WordStart:    wordCursor,
			WordEnd:      wordCursor + len(words),
			Text:         text,
		})
		wordCursor += len(words)
		paraIdx++
		buf.Reset()
		bufWords = 0
	}
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		w := len(strings.Fields(s))
		if bufWords+w > paragraphTargetWords && bufWords > 0 {
			flush()
		}
		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(s)
		bufWords += w
	}
	flush()
	return paras
}

// PopulateParagraphsForBook (re)splits every chapter of a book into
// paragraphs and writes them to the paragraphs table. Previous paragraph
// rows for the book are deleted first so re-runs produce a clean set.
// All inserts run in a single transaction for speed.
// Returns number of paragraphs written.
func PopulateParagraphsForBook(store *db.Store, bookID int64) (int, error) {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return 0, err
	}
	if len(chapters) == 0 {
		return 0, nil
	}
	// Collect all paragraphs first so the transaction is short-lived.
	var all []db.Paragraph
	for _, ch := range chapters {
		full, err := store.GetChapterContent(bookID, ch.Index)
		if err != nil || full == nil {
			continue
		}
		all = append(all, SplitIntoParagraphs(bookID, ch.Index, full.Content)...)
	}
	if err := store.ReplaceParagraphsForBook(bookID, all); err != nil {
		return 0, err
	}
	log.Printf("paragraphs: wrote %d for book %d across %d chapters", len(all), bookID, len(chapters))
	return len(all), nil
}
