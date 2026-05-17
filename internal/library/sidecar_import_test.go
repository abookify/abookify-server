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
	// words: "Hello world." [0.8s gap] "Next sentence." [0.8s gap] "End."
	// Periods on the boundary words signal sentence-end, so the gaps
	// become real paragraph breaks. (Same shape as before; just real
	// punctuation instead of single letters.)
	words := []sttWord{
		{Start: 0, End: 0.5, Word: "Hello"},
		{Start: 0.5, End: 1.0, Word: " world."},
		{Start: 1.8, End: 2.3, Word: "Next"}, // 0.8s gap, prev ends in "."
		{Start: 2.3, End: 2.8, Word: " sentence."},
		{Start: 3.6, End: 4.1, Word: "End."}, // 0.8s gap, prev ends in "."
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
	cases := []struct {
		name string
		in   []sttWord
		want string
	}{
		// "Chapter 1. The Discovery." — even with 0s gaps between words,
		// the period on "Discovery." is the title-end signal. The subtitle
		// is extracted and the base "Chapter 1" gets ": The Discovery"
		// appended. Predicate: a punctuation OR a long pause is what makes
		// a subtitle real; absence of EITHER means we don't fabricate.
		{"period-terminated subtitle (ch 1)", mk("Chapter", " 1.", " The", " Discovery."), "Chapter 1: The Discovery"},
		// Title-normalization pass converts spelled-out numbers to digits
		// so the TOC isn't a mix of "Chapter Two" + "Chapter 3" depending
		// on Whisper's transcription style. Three words, no terminator,
		// no pause → no subtitle even with a "Chapter Two" prefix.
		{"zero-gap flow = no subtitle (ch two)", mk("Chapter ", "two", " What's"), "Chapter 2"},
		{"zero-gap flow = no subtitle (part one)", mk("Part ", "One:", " This", " Thing"), "Part 1"},
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

	// WWS-style: "Chapter 2 [pause] Caffeine, Jet Lag, and Melatonin
	// [body pause] When…" — the title-end signal is the body pause AFTER
	// "Melatonin", not before. Mirrors the real data shape.
	t.Run("wws ch2 subtitle bounded by post-title pause", func(t *testing.T) {
		ws := []sttWord{
			{Start: 0.0, End: 0.3, Word: "Chapter"},
			{Start: 0.3, End: 0.6, Word: " 2"},
			{Start: 1.3, End: 1.6, Word: " Caffeine,"}, // 0.7s pause after number
			{Start: 1.6, End: 1.9, Word: " Jet"},
			{Start: 1.9, End: 2.2, Word: " Lag,"},
			{Start: 2.2, End: 2.5, Word: " and"},
			{Start: 2.5, End: 2.9, Word: " Melatonin"},
			{Start: 3.7, End: 4.0, Word: " When"}, // 0.8s pause = body
		}
		got := inferChapterTitle(ws, 0, 2)
		want := "Chapter 2: Caffeine, Jet Lag, and Melatonin"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Norm Macdonald _Based on a True Story_, ch 21: Whisper drift case.
	// "Chapter 21. The Lost Days. [2.2s real silence] I awaken from my
	// blackout..." but Whisper records "I" as starting at the same time
	// the previous word ends — the silence event's Start sits INSIDE
	// the reported "I" word's time range. Without widening the silence
	// scan past b.Start, "I" got glued onto the title.
	t.Run("norm ch21 whisper drift past b.Start caught via silence-end widening", func(t *testing.T) {
		ws := []sttWord{
			{Start: 12518.50, End: 12518.82, Word: " Chapter"},
			{Start: 12518.82, End: 12519.24, Word: " 21"},
			{Start: 12519.24, End: 12520.28, Word: " The"},
			{Start: 12520.28, End: 12520.68, Word: " Lost"},
			{Start: 12520.68, End: 12521.12, Word: " Days"},
			{Start: 12521.12, End: 12522.04, Word: " I"},      // drift!
			{Start: 12523.73, End: 12524.07, Word: " awaken"},
		}
		// Real silence runs 12521.58 → 12523.78; its start (12521.58)
		// is 0.46s into Whisper's "I" word.
		sils := []sttSilence{
			{Start: 12521.58, End: 12523.78, Duration: 2.20, Source: "silencedetect", Kind: "paragraph"},
		}
		got := inferChapterTitleWithSilences(ws, sils, 0, 21)
		want := "Chapter 21: The Lost Days"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Norm Macdonald _Based on a True Story_, ch 3: "Chapter 3, [tight]
	// My First Five Years [1.97s body pause] It doesn't take..." — the
	// narrator flows from number straight into title with NO announcement
	// pause. The old gate (require pause after number) rejected this. The
	// new gate (require any title-end signal within peek) extracts it.
	t.Run("norm ch3 tight number-to-title flow extracts via post-title pause", func(t *testing.T) {
		ws := []sttWord{
			{Start: 1537.96, End: 1538.52, Word: " Chapter"},
			{Start: 1538.52, End: 1538.88, Word: " 3,"},  // 0s gap to title
			{Start: 1538.98, End: 1539.14, Word: " My"},
			{Start: 1539.14, End: 1539.42, Word: " First"},
			{Start: 1539.42, End: 1539.66, Word: " Five"},
			{Start: 1539.66, End: 1540.22, Word: " Years"},
			{Start: 1542.19, End: 1542.75, Word: " It"}, // 1.97s pause = body
			{Start: 1542.75, End: 1543.05, Word: " doesn't"},
		}
		got := inferChapterTitle(ws, 0, 3)
		want := "Chapter 3: My First Five Years"
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

	// Norm Macdonald _Based on a True Story_, ch 23: "Chapter 23. Make a
	// Wish [0.7s] Atom..." Whisper drops the period after "Wish" and the
	// pause is shorter than the old 1.0s threshold. With the threshold
	// dropped to 0.6s the title cuts cleanly at "Wish".
	t.Run("norm ch23 short title with mid-pause cut", func(t *testing.T) {
		ws := []sttWord{
			{Start: 100.0, End: 100.4, Word: " Chapter"},
			{Start: 100.4, End: 100.8, Word: " 23."},      // announcement period
			{Start: 101.4, End: 101.7, Word: " Make"},     // 0.6s announcement pause
			{Start: 101.7, End: 101.8, Word: " a"},
			{Start: 101.8, End: 102.2, Word: " Wish"},     // no period
			{Start: 102.9, End: 103.3, Word: " Atom"},     // 0.7s pause — body
			{Start: 103.3, End: 103.6, Word: " bombs"},
		}
		got := inferChapterTitle(ws, 0, 23)
		want := "Chapter 23: Make a Wish"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Whisper sometimes absorbs the actual acoustic silence into adjacent
	// word durations, recording word.End → next.Start as 0s even when
	// there's a clear pause in the audio. The v3 silence-event stream
	// (independent ffmpeg silencedetect output) is the ground truth in
	// that case. Without consulting silences, "Make a Wish [audible 0.7s]
	// Atom" would title as "Make a Wish Atom".
	t.Run("norm ch23 whisper-recorded 0s gap rescued by silence event", func(t *testing.T) {
		ws := []sttWord{
			{Start: 100.0, End: 100.4, Word: " Chapter"},
			{Start: 100.4, End: 100.8, Word: " 23."},
			{Start: 101.4, End: 101.7, Word: " Make"},     // 0.6s announcement pause
			{Start: 101.7, End: 101.8, Word: " a"},
			{Start: 101.8, End: 102.5, Word: " Wish"},     // word stretched — End=102.5
			{Start: 102.5, End: 103.0, Word: " Atom"},     // Whisper says 0s gap…
			{Start: 103.0, End: 103.3, Word: " bombs"},
		}
		// …but ffmpeg silencedetect found a 0.7s real silence between
		// words 4 (Wish) and 5 (Atom).
		sils := []sttSilence{
			{Start: 102.5, End: 103.2, Duration: 0.7, Source: "silencedetect", Kind: "paragraph"},
		}
		got := inferChapterTitleWithSilences(ws, sils, 0, 23)
		want := "Chapter 23: Make a Wish"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Same book ch 24: "Chapter 24. Heading North [0.7s] This..."
	t.Run("norm ch24 two-word title with mid-pause cut", func(t *testing.T) {
		ws := []sttWord{
			{Start: 200.0, End: 200.4, Word: " Chapter"},
			{Start: 200.4, End: 200.8, Word: " 24."},
			{Start: 201.4, End: 201.9, Word: " Heading"},  // 0.6s announcement pause
			{Start: 201.9, End: 202.3, Word: " North"},    // no period
			{Start: 203.0, End: 203.3, Word: " This"},     // 0.7s pause — body
			{Start: 203.3, End: 203.6, Word: " is"},
		}
		got := inferChapterTitle(ws, 0, 24)
		want := "Chapter 24: Heading North"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Same book ch 5: "Chapter 5. Eight Years Old to Thirteen Years Old.
	// [0.7s] I..." — full clause with period. Even with a period the old
	// code grabbed "I" because the period+capital were on the next sentence
	// of body, not on the title. Period-cut works here, so this passes
	// regardless of TITLE_END_PAUSE — but ensures we don't regress on it.
	t.Run("norm ch5 long phrase title cut at period", func(t *testing.T) {
		ws := []sttWord{
			{Start: 300.0, End: 300.4, Word: " Chapter"},
			{Start: 300.4, End: 300.8, Word: " 5."},
			{Start: 301.4, End: 301.6, Word: " Eight"},
			{Start: 301.6, End: 302.0, Word: " Years"},
			{Start: 302.0, End: 302.2, Word: " Old"},
			{Start: 302.2, End: 302.3, Word: " to"},
			{Start: 302.3, End: 302.7, Word: " Thirteen"},
			{Start: 302.7, End: 303.0, Word: " Years"},
			{Start: 303.0, End: 303.4, Word: " Old."},  // period
			{Start: 304.1, End: 304.3, Word: " I"},
			{Start: 304.3, End: 304.5, Word: " was"},
		}
		got := inferChapterTitle(ws, 0, 5)
		want := "Chapter 5: Eight Years Old to Thirteen Years Old"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Bonfire ch26 pattern: narrator runs a single body pronoun ("It") into
	// the title without a real pause, then a body pause after. Without the
	// trim we'd ship "Death, New York Style It" in the TOC. The trim
	// removes the trailing bleed because the title ended on a silence, not
	// a punctuation mark, and "It" is a sentence-starter.
	t.Run("bonfire ch26 trailing It bled in from body", func(t *testing.T) {
		ws := []sttWord{
			{Start: 400.0, End: 400.4, Word: " Chapter"},
			{Start: 400.4, End: 400.8, Word: " 26."},
			{Start: 401.4, End: 401.7, Word: " Death,"},
			{Start: 401.7, End: 402.0, Word: " New"},
			{Start: 402.0, End: 402.2, Word: " York"},
			{Start: 402.2, End: 402.6, Word: " Style"},
			{Start: 402.6, End: 402.8, Word: " It"},
			{Start: 403.5, End: 403.8, Word: " happened"},
		}
		got := inferChapterTitle(ws, 0, 26)
		want := "Chapter 26: Death, New York Style"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Bonfire ch7 pattern: all-caps narrator delivery — "CATCHING THE FISH"
	// — with a trailing "The" bled in. Trim the bleed first, then the
	// recased-from-all-caps subtitle becomes "Catching the Fish".
	t.Run("bonfire ch7 all-caps title with bled-in The", func(t *testing.T) {
		ws := []sttWord{
			{Start: 500.0, End: 500.4, Word: " CHAPTER"},
			{Start: 500.4, End: 500.8, Word: " SEVEN."},
			{Start: 501.4, End: 501.7, Word: " CATCHING"},
			{Start: 501.7, End: 502.0, Word: " THE"},
			{Start: 502.0, End: 502.3, Word: " FISH"},
			{Start: 502.3, End: 502.5, Word: " The"},
			{Start: 503.4, End: 503.7, Word: " day"},
		}
		got := inferChapterTitle(ws, 0, 7)
		want := "Chapter 7: Catching the Fish"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Trim must NOT misfire when the title legitimately ends with a content
	// word — "Chapter 5: A Study in Scarlet" should keep "Scarlet" because
	// it's not in the bleedInStarters allowlist.
	t.Run("legit title ending on content word is preserved", func(t *testing.T) {
		ws := []sttWord{
			{Start: 600.0, End: 600.4, Word: " Chapter"},
			{Start: 600.4, End: 600.8, Word: " 5."},
			{Start: 601.4, End: 601.6, Word: " A"},
			{Start: 601.6, End: 602.0, Word: " Study"},
			{Start: 602.0, End: 602.2, Word: " in"},
			{Start: 602.2, End: 602.6, Word: " Scarlet"},
			{Start: 603.4, End: 603.7, Word: " The"}, // body pause + body
		}
		got := inferChapterTitle(ws, 0, 5)
		want := "Chapter 5: A Study in Scarlet"
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
	// Punctuation-gated: the word before each silence must end with .!?
	// for the break to take. "two." and "four." carry the terminator.
	words := []sttWord{
		{Start: 0.0, End: 0.3, Word: "One"},
		{Start: 0.3, End: 0.6, Word: " two."},
		// paragraph silence 0.6-1.4 (0.8s) — "two." ends a sentence
		{Start: 1.4, End: 1.7, Word: " Three"},
		{Start: 1.7, End: 2.0, Word: " four."},
		// paragraph silence 2.0-3.0 (1.0s) — "four." ends a sentence
		{Start: 3.0, End: 3.3, Word: " Five"},
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
	// "world." carries a period — the punctuation gate accepts the break.
	words := []sttWord{
		{Start: 0.0, End: 0.3, Word: "Hello"},
		{Start: 0.3, End: 0.6, Word: " world."},
		{Start: 0.6, End: 0.9, Word: " Next"}, // Whisper says no gap, but silence event says 0.7s pause
		{Start: 0.9, End: 1.2, Word: " sentence."},
	}
	silences := []sttSilence{
		{Start: 0.55, End: 0.85, Duration: 0.30, Kind: "sentence"},
		{Start: 0.55, End: 0.85, Duration: 0.70, Kind: "paragraph"}, // the real one
	}
	// v2 path: paragraph break after "world." (word 1, before word 2).
	got := buildChapterContentByIdxWithSilences(words, silences, 0, 4)
	want := "Hello world.\n\nNext sentence."
	if got != want {
		t.Errorf("v2 builder: got %q, want %q", got, want)
	}
	// v1 path: no silences → word-gap math → no break (all 0s gaps).
	gotV1 := buildChapterContentByIdxWithSilences(words, nil, 0, 4)
	wantV1 := "Hello world. Next sentence."
	if gotV1 != wantV1 {
		t.Errorf("v1 builder: got %q, want %q", gotV1, wantV1)
	}
}

// Regression: Norm Macdonald ch21 had paragraph-grade silences sitting
// inside the chapter title region ("Chapter 21 [0.66s] The Lost Days I
// [2.2s] awaken..."), producing reader text like
//   Chapter 21 The
//
//   Lost Days I
//
//   awaken from my blackout...
// The punctuation gate on detectParagraphsFromSilences and
// buildChapterContentByIdxWithSilences kills those breaks because "The"
// and "I" don't end in .!? — the only break that survives is the real
// one at "hangover." → "As always..." (where the word ends in a period).
func TestSilenceParagraphBreaksRequireSentenceEnd(t *testing.T) {
	words := []sttWord{
		{Start: 0.00, End: 0.32, Word: " Chapter"},
		{Start: 0.32, End: 0.74, Word: " 21"},
		// silence 1: 0.74-1.40 (paragraph kind) — sits between "21" and "The".
		// "21" has no terminator → should NOT cause a paragraph break.
		{Start: 1.40, End: 1.62, Word: " The"},
		{Start: 1.62, End: 1.92, Word: " Lost"},
		{Start: 1.92, End: 2.24, Word: " Days"},
		{Start: 2.24, End: 2.54, Word: " I"},
		// silence 2: 2.54-4.84 (paragraph kind, real title-to-body pause).
		// "I" has no terminator → should NOT cause a paragraph break either
		// (the title-end silence is consumed by the title extractor, not the
		// paragraph splitter).
		{Start: 4.84, End: 5.10, Word: " awaken"},
		{Start: 5.10, End: 5.30, Word: " from"},
		{Start: 5.30, End: 5.50, Word: " my"},
		{Start: 5.50, End: 5.90, Word: " blackout"},
		{Start: 5.90, End: 6.10, Word: " without"},
		{Start: 6.10, End: 6.62, Word: " hangover."},
		// silence 3: 6.62-7.30 (paragraph kind, real body sentence break).
		// "hangover." ends a sentence → SHOULD cause a paragraph break.
		{Start: 7.30, End: 7.50, Word: " As"},
		{Start: 7.50, End: 7.80, Word: " always."},
	}
	sils := []sttSilence{
		{Start: 0.74, End: 1.40, Duration: 0.66, Kind: "paragraph"},
		{Start: 2.54, End: 4.84, Duration: 2.30, Kind: "paragraph"},
		{Start: 6.62, End: 7.30, Duration: 0.68, Kind: "paragraph"},
	}
	got := buildChapterContentByIdxWithSilences(words, sils, 0, len(words))
	want := "Chapter 21 The Lost Days I awaken from my blackout without hangover.\n\nAs always."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}

	sc := &sttSidecar{Version: 2, Words: words, Silences: sils}
	paras := detectParagraphsFromSilences(sc, 0, len(words))
	if len(paras) != 2 {
		t.Fatalf("want 2 paragraphs, got %d: %+v", len(paras), paras)
	}
	// Both paragraphs are chapter-local word-index ranges. The break lands
	// at the first word AFTER the silence ("As" at index 12).
	if paras[0].start != 0 || paras[0].end != 12 || paras[1].start != 12 || paras[1].end != 14 {
		t.Errorf("para boundaries wrong: got %+v", paras)
	}
}

// Content builder should insert \n\n at pause boundaries so the FE can
// split on double-newline.
func TestBuildChapterContentByIdx_InsertsParagraphBreaks(t *testing.T) {
	// "world." carries a period — the v1 word-gap path's punctuation
	// gate accepts the break.
	words := []sttWord{
		{Start: 0, End: 0.5, Word: "Hello"},
		{Start: 0.5, End: 1.0, Word: " world."},
		{Start: 1.8, End: 2.3, Word: "Next"}, // 0.8s gap → paragraph break
		{Start: 2.3, End: 2.8, Word: " sentence."},
	}
	content := buildChapterContentByIdx(words, 0, len(words))
	want := "Hello world.\n\nNext sentence."
	if content != want {
		t.Errorf("want %q, got %q", want, content)
	}
}
