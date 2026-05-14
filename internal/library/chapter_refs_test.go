package library

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// noPrelude: 30 chapters numbered "Chapter 1".."Chapter 30" with no
// prefatory section. "Chapter N" maps to chapter_idx N-1.
func noPreludeChapters() []db.Chapter {
	out := make([]db.Chapter, 30)
	for i := range out {
		out[i] = db.Chapter{Index: i, Title: "Chapter " + strconv.Itoa(i+1)}
	}
	return out
}

// withPrelude: idx 0 is "Prelude", idx 1..30 are "Chapter 1".."Chapter 30".
// Mirrors Norm Macdonald _Based on a True Story_. "Chapter 26" maps to
// chapter_idx 26, NOT 25 — the bug this test guards against.
func withPreludeChapters() []db.Chapter {
	out := make([]db.Chapter, 31)
	out[0] = db.Chapter{Index: 0, Title: "Prelude"}
	for i := 1; i < 31; i++ {
		out[i] = db.Chapter{Index: i, Title: "Chapter " + strconv.Itoa(i)}
	}
	return out
}

func TestParseChapterRefs(t *testing.T) {
	chapters := noPreludeChapters()

	cases := []struct {
		name string
		q    string
		want []int
	}{
		{"numeric", "summarize chapter 26", []int{25}},
		{"abbrev", "what happens in ch 5", []int{4}},
		{"abbrev with period", "ch. 12 reaction", []int{11}},
		{"chap variant", "chap 7 mood", []int{6}},
		{"word number", "what is chapter twenty-six about", []int{25}},
		{"word number two-token", "tell me about chapter twenty six", []int{25}},
		{"single-digit word", "summarize chapter five", []int{4}},
		{"multiple refs in one question", "compare chapter 5 and chapter 12", []int{4, 11}},
		{"capitalized", "What did Chapter 3 mean?", []int{2}},
		{"out-of-range high", "summarize chapter 99", nil},
		{"out-of-range zero", "what is chapter 0 about", nil},
		{"no ref", "tell me about Norm's childhood", nil},
		{"unrelated number", "Norm was 26 in this book", nil},
		{"anaphoric ignored for now", "what is this chapter about", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseChapterRefs(c.q, chapters)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseChapterRefs(%q) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

// Regression: when the book has a non-numbered prelude at idx 0,
// "Chapter 26" must map to chapter_idx 26, not idx 25 (which would be
// "Chapter 25" in a prelude-bearing book). This bug shipped in the
// initial chapter-name boost — Norm asked for ch 26 and got ch 25's
// content.
func TestParseChapterRefs_PreludeOffset(t *testing.T) {
	chapters := withPreludeChapters()
	cases := []struct {
		q    string
		want []int
	}{
		{"summarize chapter 26", []int{26}},
		{"summarize chapter twenty-six", []int{26}},
		{"what is chapter 1 about", []int{1}},
		{"compare chapter 5 and chapter 12", []int{5, 12}},
	}
	for _, c := range cases {
		got := ParseChapterRefs(c.q, chapters)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("with-prelude: ParseChapterRefs(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}
