package library

import (
	"testing"
)

// detectChaptersFromPauses should flag the word immediately after any gap
// >= CHAPTER_PAUSE_SECS as a new chapter start, plus always produce a
// chapter 1 starting at word 0.
func TestDetectChaptersFromPauses(t *testing.T) {
	// words: "intro one two" [4s gap] "chapter two words here" [4s gap] "end"
	words := []sttWord{
		{Start: 0, End: 1, Word: "intro"},
		{Start: 1, End: 2, Word: " one"},
		{Start: 2, End: 3, Word: " two"},
		{Start: 7, End: 8, Word: "chapter"}, // 4s gap from prev end (3s → 7s)
		{Start: 8, End: 9, Word: " two"},
		{Start: 9, End: 10, Word: " words"},
		{Start: 10, End: 11, Word: " here"},
		{Start: 15, End: 16, Word: "end"}, // 4s gap from prev end (11s → 15s)
	}
	chs := detectChaptersFromPauses(words)
	if len(chs) != 3 {
		t.Fatalf("want 3 chapters (including start), got %d: %+v", len(chs), chs)
	}
	if chs[0].WordIdx != 0 {
		t.Errorf("chapter 1 should start at word 0, got %d", chs[0].WordIdx)
	}
	if chs[1].WordIdx != 3 {
		t.Errorf("chapter 2 should start at word 3 (post-first-gap), got %d", chs[1].WordIdx)
	}
	if chs[2].WordIdx != 7 {
		t.Errorf("chapter 3 should start at word 7 (post-second-gap), got %d", chs[2].WordIdx)
	}
}

// detectParagraphsFromPauses splits a [start,end) range by medium gaps.
func TestDetectParagraphsFromPauses(t *testing.T) {
	// words: "a b" [0.8s gap] "c d" [0.8s gap] "e"
	words := []sttWord{
		{Start: 0, End: 0.5, Word: "a"},
		{Start: 0.5, End: 1.0, Word: " b"},
		{Start: 1.8, End: 2.3, Word: "c"}, // 0.8s gap
		{Start: 2.3, End: 2.8, Word: " d"},
		{Start: 3.6, End: 4.1, Word: "e"}, // 0.8s gap
	}
	paras := detectParagraphsFromPauses(words, 0, len(words))
	if len(paras) != 3 {
		t.Fatalf("want 3 paragraphs, got %d: %+v", len(paras), paras)
	}
	// Each paragraph should be local word indexes (0-based within the
	// chapter range, which here is the whole slice).
	expected := [][]int{{0, 2}, {2, 4}, {4, 5}}
	for i, ex := range expected {
		if paras[i].start != ex[0] || paras[i].end != ex[1] {
			t.Errorf("paragraph %d: want [%d,%d), got [%d,%d)", i, ex[0], ex[1], paras[i].start, paras[i].end)
		}
	}
}

// No gap → one paragraph covering the whole range.
func TestDetectParagraphsFromPauses_NoGaps(t *testing.T) {
	words := []sttWord{
		{Start: 0, End: 0.5, Word: "a"},
		{Start: 0.5, End: 1.0, Word: " b"},
		{Start: 1.1, End: 1.6, Word: " c"},
	}
	paras := detectParagraphsFromPauses(words, 0, 3)
	if len(paras) != 1 {
		t.Fatalf("want 1 paragraph, got %d", len(paras))
	}
	if paras[0].start != 0 || paras[0].end != 3 {
		t.Errorf("want [0,3), got [%d,%d)", paras[0].start, paras[0].end)
	}
}

func TestInferChapterTitle(t *testing.T) {
	mk := func(ss ...string) []sttWord {
		out := make([]sttWord, len(ss))
		for i, s := range ss {
			out[i] = sttWord{Word: s}
		}
		return out
	}
	cases := []struct {
		name string
		in   []sttWord
		want string
	}{
		{"chapter cuts at period", mk("Chapter ", "1", ".", " The", " Discovery", ".", " Next"), "Chapter 1"},
		{"chapter numbered only", mk("Chapter ", "two"), "Chapter two"},
		{"part with colon subtitle kept", mk("Part ", "One", ":", " This", " Thing"), "Part One: This Thing"},
		{"single-word section", mk("Foreword", ".", " Content"), "Foreword"},
		{"preface", mk("Preface"), "Preface"},
		{"acknowledgments", mk("Acknowledgments"), "Acknowledgments"},
		{"snippet fallback", mk("To ", "Dacca ", "Keltner", ",", " for", " help", ",", " for", " inspiring"), "Ch 1 · To Dacca Keltner , for help , for…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inferChapterTitle(c.in, 0, 1)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Content builder should insert \n\n at pause boundaries so the FE can
// split on double-newline.
func TestBuildChapterContentByIdx_InsertsParagraphBreaks(t *testing.T) {
	words := []sttWord{
		{Start: 0, End: 0.5, Word: "hello"},
		{Start: 0.5, End: 1.0, Word: " world"},
		{Start: 1.8, End: 2.3, Word: "next"}, // 0.8s gap → paragraph break
		{Start: 2.3, End: 2.8, Word: " sentence"},
	}
	content := buildChapterContentByIdx(words, 0, len(words))
	want := "hello world\n\nnext sentence"
	if content != want {
		t.Errorf("want %q, got %q", want, content)
	}
}
