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
	// mk builds word slices with zero gaps between them (default case).
	// For tests that need an announcement pause, override gaps manually.
	mk := func(ss ...string) []sttWord {
		out := make([]sttWord, len(ss))
		t := 0.0
		for i, s := range ss {
			out[i] = sttWord{Start: t, End: t + 0.3, Word: s}
			t += 0.3
		}
		return out
	}
	// mkWithGap builds a word slice with a specific gap inserted after index `afterIdx`.
	mkWithGap := func(afterIdx int, gapSec float64, ss ...string) []sttWord {
		out := make([]sttWord, len(ss))
		t := 0.0
		for i, s := range ss {
			out[i] = sttWord{Start: t, End: t + 0.3, Word: s}
			t += 0.3
			if i == afterIdx {
				t += gapSec // inject the announcement gap
			}
		}
		return out
	}
	cases := []struct {
		name string
		in   []sttWord
		want string
	}{
		// All mk() cases have 0s gaps between words → no announcement pause
		// → narrator flowed to body → return just "Chapter N". These tests
		// reflect the new pause-based contract.
		{"zero-gap flow = no subtitle (ch 1)", mk("Chapter", " 1.", " The", " Discovery."), "Chapter 1"},
		{"zero-gap flow = no subtitle (ch two)", mk("Chapter ", "two", " What's"), "Chapter two"},
		{"zero-gap flow = no subtitle (part one)", mk("Part ", "One:", " This", " Thing"), "Part One"},
		{"single-word section", mk("Foreword", ".", " Content"), "Foreword"},
		{"preface", mk("Preface"), "Preface"},
		{"acknowledgments", mk("Acknowledgments"), "Acknowledgments"},
		// Snippet fallback now returns just "Chapter N" — no fabricated
		// snippet titles. We only promote to "Chapter N: Title" when the
		// narrator clearly announced a chapter (e.g. "Two." then title).
		{"snippet fallback", mk("To ", "Dacca ", "Keltner", ",", " for", " help", ",", " for", " inspiring"), "Chapter 1"},
	}
	// PHM-style: Whisper gives 0s gap between "1" and "What's" (interpolated
	// within a single segment) AND no period on "1". Without either signal
	// we assume no subtitle — narrator flowed into body.
	t.Run("phm style tight flow no subtitle", func(t *testing.T) {
		ws := []sttWord{
			{Start: 0.0, End: 0.36, Word: " Chapter"},
			{Start: 0.36, End: 0.62, Word: " 1"},       // no period
			{Start: 0.62, End: 1.90, Word: " What's"},  // zero-gap (per Whisper)
		}
		got := inferChapterTitle(ws, 0, 1)
		want := "Chapter 1"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// WWS-style alt: "Chapter 2 [0.96s cross-segment gap] Caffeine, Jet Lag,
	// and Melatonin…" — no period on "2" but the 0.96s gap is real (Whisper
	// segmented there). Should trigger subtitle extraction.
	t.Run("wws ch2 no period but real cross-segment gap", func(t *testing.T) {
		ws := mkWithGap(1, 0.7, "Chapter", " 2", " Caffeine,", " Jet", " Lag,", " and", " Melatonin")
		got := inferChapterTitle(ws, 0, 2)
		want := "Chapter 2: Caffeine, Jet Lag, and Melatonin"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// WWS-style: "Chapter 4. Ape Beds, Dinosaurs, and Napping with Half a
	// Brain. They ..." — announcement period after "4", subtitle follows,
	// cut at next period-terminated token (Brain.).
	t.Run("wws style subtitle after announcement period", func(t *testing.T) {
		ws := []sttWord{
			{Start: 7757.2, End: 7757.9, Word: " Chapter"},
			{Start: 7757.9, End: 7758.3, Word: " 4."},       // period — subtitle follows
			{Start: 7758.9, End: 7759.6, Word: " Ape"},       // 0.6s gap
			{Start: 7759.6, End: 7760.1, Word: " Beds,"},
			{Start: 7760.2, End: 7760.9, Word: " Dinosaurs,"},
			{Start: 7761.2, End: 7761.6, Word: " and"},
			{Start: 7761.6, End: 7762.0, Word: " Napping"},
			{Start: 7762.0, End: 7762.2, Word: " with"},
			{Start: 7762.2, End: 7762.5, Word: " Half"},
			{Start: 7762.5, End: 7762.6, Word: " a"},
			{Start: 7762.6, End: 7763.0, Word: " Brain."},  // period → cut here
			{Start: 7763.5, End: 7764.0, Word: " They"},
		}
		got := inferChapterTitle(ws, 0, 4)
		want := "Chapter 4: Ape Beds, Dinosaurs, and Napping with Half a Brain"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Retired: the legacy "cut at first big pause anywhere" rule used to
	// accept tight-read titles that then paused before body. That rule gave
	// lots of false positives where mid-body pauses got treated as title
	// endings. The new rule requires a pause >= 0.5s immediately AFTER the
	// chapter number — an announcement pause is the reliable signal of a
	// subtitle. See the "wws ch2 no period but real cross-segment gap" test
	// for the positive case.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := inferChapterTitle(c.in, 0, 1)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// v2 silence-based chapter detection: chapter-kind silences become the
// chapter boundaries. First chapter is always at word 0.
func TestDetectChaptersFromSilences_V2(t *testing.T) {
	// 10 words, two chapter-grade silences between words 3-4 and 7-8.
	words := []sttWord{
		{Start: 0.0, End: 0.3, Word: "one"},
		{Start: 0.3, End: 0.6, Word: " two"},
		{Start: 0.6, End: 0.9, Word: " three"},
		{Start: 0.9, End: 1.2, Word: " four"},
		// chapter silence 1.2-5.0
		{Start: 5.0, End: 5.3, Word: " five"},
		{Start: 5.3, End: 5.6, Word: " six"},
		{Start: 5.6, End: 5.9, Word: " seven"},
		{Start: 5.9, End: 6.2, Word: " eight"},
		// chapter silence 6.2-10.0
		{Start: 10.0, End: 10.3, Word: " nine"},
		{Start: 10.3, End: 10.6, Word: " ten"},
	}
	sc := &sttSidecar{
		Version:  2,
		Duration: 11.0,
		Words:    words,
		Silences: []sttSilence{
			{Start: 1.2, End: 5.0, Duration: 3.8, Kind: "chapter"},
			{Start: 6.2, End: 10.0, Duration: 3.8, Kind: "chapter"},
		},
	}
	chs := detectChaptersFromSilences(sc)
	if len(chs) != 3 {
		t.Fatalf("want 3 chapters (word 0 + 2 silence boundaries), got %d", len(chs))
	}
	if chs[0].WordIdx != 0 {
		t.Errorf("chapter 1 WordIdx: got %d want 0", chs[0].WordIdx)
	}
	if chs[1].WordIdx != 4 {
		t.Errorf("chapter 2 WordIdx: got %d want 4 (first word after silence 1)", chs[1].WordIdx)
	}
	if chs[2].WordIdx != 8 {
		t.Errorf("chapter 3 WordIdx: got %d want 8 (first word after silence 2)", chs[2].WordIdx)
	}
}

// v2 paragraph detection returns chapter-local word-index ranges bounded
// by paragraph-grade silence events.
func TestDetectParagraphsFromSilences_V2(t *testing.T) {
	words := []sttWord{
		{Start: 0.0, End: 0.3, Word: "one"},
		{Start: 0.3, End: 0.6, Word: " two"},
		// paragraph silence 0.6-1.4 (0.8s)
		{Start: 1.4, End: 1.7, Word: " three"},
		{Start: 1.7, End: 2.0, Word: " four"},
		// paragraph silence 2.0-3.0 (1.0s)
		{Start: 3.0, End: 3.3, Word: " five"},
	}
	sc := &sttSidecar{
		Version: 2,
		Words:   words,
		Silences: []sttSilence{
			{Start: 0.6, End: 1.4, Duration: 0.8, Kind: "paragraph"},
			{Start: 2.0, End: 3.0, Duration: 1.0, Kind: "paragraph"},
		},
	}
	paras := detectParagraphsFromSilences(sc, 0, 5)
	if len(paras) != 3 {
		t.Fatalf("want 3 paragraphs, got %d: %+v", len(paras), paras)
	}
	want := [][]int{{0, 2}, {2, 4}, {4, 5}}
	for i, w := range want {
		if paras[i].start != w[0] || paras[i].end != w[1] {
			t.Errorf("para %d: got [%d,%d), want [%d,%d)", i, paras[i].start, paras[i].end, w[0], w[1])
		}
	}
}

// v2 content builder uses silence events for paragraph breaks (real
// acoustic) not word gaps (interpolated by Whisper).
func TestBuildChapterContentV2_UsesSilences(t *testing.T) {
	// Words with ZERO gaps (simulating Whisper within-segment interpolation).
	// v1 would miss the paragraph break; v2 catches it via the silence event.
	words := []sttWord{
		{Start: 0.0, End: 0.3, Word: "hello"},
		{Start: 0.3, End: 0.6, Word: " world"},
		{Start: 0.6, End: 0.9, Word: " next"}, // Whisper says no gap, but silence event says 0.7s pause
		{Start: 0.9, End: 1.2, Word: " sentence"},
	}
	silences := []sttSilence{
		{Start: 0.55, End: 0.85, Duration: 0.30, Kind: "sentence"},
		{Start: 0.55, End: 0.85, Duration: 0.70, Kind: "paragraph"}, // the real one
	}
	// v2 path: paragraph break after "world" (word 1, before word 2).
	got := buildChapterContentByIdxWithSilences(words, silences, 0, 4)
	want := "hello world\n\nnext sentence"
	if got != want {
		t.Errorf("v2 builder: got %q, want %q", got, want)
	}
	// v1 path: no silences → word-gap math → no break (all 0s gaps).
	gotV1 := buildChapterContentByIdxWithSilences(words, nil, 0, 4)
	wantV1 := "hello world next sentence"
	if gotV1 != wantV1 {
		t.Errorf("v1 builder: got %q, want %q", gotV1, wantV1)
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
