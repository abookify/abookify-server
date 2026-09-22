package library

import (
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// ListenStart picks the audio file a FIRST press of play should land on: the
// first file of the displayed edition that is prose, not front matter. A
// stranger who takes our Dracula sample and presses play must not hear the
// Gutenberg title page and colophon read aloud for 26 s as their first taste
// of word-level read-along (stranger walk, 2026-09-21). Once a saved position
// exists the client resumes there and this is not consulted.
//
// Leading files are skipped while they look like front matter: shorter than
// two minutes, titled with the book's own name, or titled as contents /
// preface / dedication and the like. The first survivor wins; if every file
// is skipped, file 0. Nothing here rewrites the TOC — the rows stay, only
// the first press of play moves.
func ListenStart(work *db.Work) *db.Book {
	if work == nil || len(work.AudioFiles) == 0 {
		return nil
	}
	display := ResolveDisplayAudio(work)
	var files []*db.Book
	for i := range work.AudioFiles {
		b := &work.AudioFiles[i]
		if b.Visibility == "internal" {
			continue
		}
		if display != nil && (b.Origin != display.Origin || b.Edition != display.Edition) {
			continue
		}
		files = append(files, b)
	}
	if len(files) == 0 {
		return nil
	}
	workTitle := normTitle(work.Title)
	for k, b := range files {
		var next *db.Book
		if k+1 < len(files) {
			next = files[k+1]
		}
		if isFrontMatterFile(b, workTitle, next) {
			continue
		}
		return b
	}
	return files[0]
}

const frontMatterMaxSecs = 120

var frontMatterTitle = regexp.MustCompile(`^(?:table of contents|contents|front matter|title page|colophon|dedication|preface|foreword|introduction|acknowledg(?:e)?ments?|about (?:the|this)|copyright|credits|note|a note|publisher'?s note|editor'?s note|chapter \d+)$`)

// firstChapterTitle: "Chapter I", "CHAPTER 1", "Stave One", "Book One", "Letter 1",
// "I. A Scandal in Bohemia", "1 Into the Primitive" — the book's first unit.
var firstChapterTitle = regexp.MustCompile(`^(?:(?:chapter|stave|book|part|letter|canto)\s+)?(?:1|i|one|first)(?:\s|$)`)

func isFrontMatterFile(b *db.Book, workTitle string, next *db.Book) bool {
	t := normTitle(b.Title)
	if b.Duration > 0 && b.Duration < frontMatterMaxSecs {
		return true
	}
	if t == "" {
		return false
	}
	if workTitle != "" && (t == workTitle || strings.HasPrefix(workTitle, t) && len(t) >= 6) {
		// A file carrying the book's own name is the title page + preface when the
		// FIRST chapter follows it (Pride and Prejudice: 30 min of front matter,
		// then "Chapter I."). When chapter TWO follows, this file IS chapter one
		// wearing the running head as its title (Dracula) — keep it.
		return next != nil && firstChapterTitle.MatchString(normTitle(next.Title))
	}
	return frontMatterTitle.MatchString(t)
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9 ]+`)
var spaces = regexp.MustCompile(`\s+`)

// normTitle lowercases, strips punctuation, and collapses the letter-spaced
// "D R A C U L A" that Gutenberg title pages carry into "dracula".
func normTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", " ")
	s = nonAlnum.ReplaceAllString(s, " ")
	s = spaces.ReplaceAllString(strings.TrimSpace(s), " ")
	// letter-spaced: every token is a single character → join them
	toks := strings.Fields(s)
	if len(toks) >= 3 {
		single := true
		for _, t := range toks {
			if len(t) != 1 {
				single = false
				break
			}
		}
		if single {
			return strings.Join(toks, "")
		}
	}
	return s
}
