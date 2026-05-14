// Detect explicit chapter references in a Q&A question and pull the
// matching chunks. Used to "boost" retrieval when the user names a
// chapter outright — pure vector similarity often misses the named
// chapter because the question's words ("summarize chapter 26") don't
// resemble the chapter's prose, so without this boost the LLM sees
// chunks unrelated to chapter 26 and can't answer.
//
// Same parser is the foundation for #130's scope picker — when the
// user mentions a chapter, we eventually want to update the visible
// scope, not just silently boost retrieval. Keeping the parser as a
// reusable function makes that an easy follow-on.
package library

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// numericChapterRe matches "chapter 26", "Chapter 26", "ch 26", "ch. 26",
// "chap 26" — case-insensitive, capture group 1 is the digits. Trailing
// non-digit boundary keeps it from matching "chapter 26th" weirdly.
var numericChapterRe = regexp.MustCompile(`(?i)\b(?:chapter|chap|ch\.?)\s+(\d{1,3})\b`)

// wordChapterRe matches "chapter twenty-six", "chapter five", "chapter
// twenty six". Captures 1-2 words after the prefix; we parse them with
// the existing word-number machinery from chapter_detect.go.
var wordChapterRe = regexp.MustCompile(`(?i)\b(?:chapter|chap|ch\.?)\s+([a-z\-]+(?:\s+[a-z\-]+)?)\b`)

// titleNumberRe pulls the human-facing chapter number out of a stored
// chapter title. Matches "Chapter 26", "Chapter 26: Subtitle", "Ch 26",
// "Part 3" — capture group 1 is the digits.
var titleNumberRe = regexp.MustCompile(`(?i)^(?:chapter|part|book|ch\.?|chap)\s+(\d{1,3})\b`)

// buildChapterNumberMap returns a map of human-facing chapter number →
// 0-based chapter_idx. Accounts for books that start with non-numbered
// prelude/foreword sections (Norm Macdonald: idx 0 = "Prelude", idx 1 =
// "Chapter 1", … idx 26 = "Chapter 26"). Without parsing the title, a
// blind "human - 1" mapping would shift every reference by one.
//
// Falls back to "human - 1" for chapters whose titles don't carry a
// parseable number — covers books with cleanly numbered chapters but no
// prelude.
func buildChapterNumberMap(chapters []db.Chapter) map[int]int {
	m := map[int]int{}
	for _, c := range chapters {
		if mm := titleNumberRe.FindStringSubmatch(strings.TrimSpace(c.Title)); mm != nil {
			if n, err := strconv.Atoi(mm[1]); err == nil {
				if _, exists := m[n]; !exists { // first match wins
					m[n] = c.Index
				}
			}
		}
	}
	return m
}

// ParseChapterRefs scans the question for explicit chapter references
// and returns 0-based chapter indices that exist in the given chapter
// list. "Chapter 26" → index 25. Multiple references in one question
// (e.g. "compare chapter 5 and chapter 12") all come back. De-duplicated.
//
// Only handles numbered/word-numbered references. Anaphoric ones
// ("this chapter", "the last few chapters") need playback-position
// context and live in #130.
func ParseChapterRefs(question string, chapters []db.Chapter) []int {
	if question == "" || len(chapters) == 0 {
		return nil
	}
	maxIndex := 0
	for _, c := range chapters {
		if c.Index > maxIndex {
			maxIndex = c.Index
		}
	}

	// Resolve human chapter number → DB chapter_idx using the actual
	// chapter titles. Norm Macdonald's book starts with a "Prelude" at
	// idx 0, so "Chapter 26" is at idx 26, not 25. Books without any
	// prelude give "Chapter 1" at idx 0, so "Chapter N" is at idx N-1.
	numberToIdx := buildChapterNumberMap(chapters)

	seen := map[int]bool{}
	var out []int

	add := func(humanNumber int) {
		idx, ok := numberToIdx[humanNumber]
		if !ok {
			// Title-based map didn't include this number (chapter title
			// not parseable, or the book uses unconventional titles).
			// Fall back to the simple 1-based offset.
			idx = humanNumber - 1
		}
		if idx < 0 || idx > maxIndex {
			return
		}
		if seen[idx] {
			return
		}
		seen[idx] = true
		out = append(out, idx)
	}

	for _, m := range numericChapterRe.FindAllStringSubmatch(question, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			add(n)
		}
	}

	for _, m := range wordChapterRe.FindAllStringSubmatch(question, -1) {
		// Skip cases where the regex also matched a numeric form — the
		// numeric handler already added them, and "chapter 26" actually
		// matches both regexes (the word-form captures "26" as a word).
		if _, err := strconv.Atoi(strings.Fields(m[1])[0]); err == nil {
			continue
		}
		tokens := strings.Fields(strings.ReplaceAll(strings.ToLower(m[1]), "-", " "))
		// parseTitleNumber lives in chapter_detect.go and handles
		// "twenty six" → 26 etc.
		if n, _ := parseTitleNumber(tokens); n > 0 {
			add(n)
		}
	}

	return out
}

// FetchChapterChunks loads all chunks for the given chapter indices
// from a single book. Used to force-include named chapters in retrieval
// regardless of vector similarity. Order: chapter_idx ASC, chunk_idx ASC.
func FetchChapterChunks(store *db.Store, bookID int64, chapterIndices []int) ([]db.Chunk, error) {
	if len(chapterIndices) == 0 {
		return nil, nil
	}
	want := map[int]bool{}
	for _, i := range chapterIndices {
		want[i] = true
	}
	all, err := store.ListChunks(bookID)
	if err != nil {
		return nil, err
	}
	var out []db.Chunk
	for _, c := range all {
		if want[c.ChapterIdx] {
			out = append(out, c)
		}
	}
	return out, nil
}
