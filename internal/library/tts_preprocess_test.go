package library

import (
	"strings"
	"testing"
)

// Gutenberg chapters embed their own heading; prepending the title again made
// the narrator open with "Stave One. Stave One. Marley's Ghost." — the first
// thirty seconds a starter-book listener hears.
func TestPreprocessSkipsDuplicateTitleAnnouncement(t *testing.T) {
	got := PreprocessForTTS("STAVE  ONE.", "STAVE ONE. MARLEY'S GHOST.\n\nMarley was dead: to begin with.")
	low := strings.ToLower(got)
	if strings.Count(low, "stave one") != 1 {
		t.Errorf("title announced twice:\n%s", got)
	}
	// A chapter whose body does NOT open with the title keeps the announcement.
	got = PreprocessForTTS("Chapter 2", "It was the best of times.")
	if !strings.HasPrefix(got, "Chapter 2") {
		t.Errorf("announcement lost for non-duplicated title: %q", got)
	}
}

// The segments must reproduce PreprocessForTTS's words exactly (word-sync
// depends on it) while carrying the pause structure: long after a spoken
// title, medium between paragraphs, none at chapter end.
func TestPreprocessSegmentsMatchJoinedOutput(t *testing.T) {
	title := "STAVE ONE. MARLEY'S GHOST."
	content := "Marley was dead: to begin with.\n\nThere is no doubt whatever about that.\n\nOld Marley was as dead as a door-nail."
	segs := PreprocessForTTSSegments(title, content)
	if len(segs) < 3 {
		t.Fatalf("want title + paragraphs, got %d segments", len(segs))
	}
	var joined []string
	for _, s := range segs {
		joined = append(joined, s.Text)
	}
	// Compare WORD STREAMS, not bytes — word-sync counts words; whitespace
	// warts in the joined path (a stray continuation space) don't matter.
	got := strings.Fields(strings.Join(joined, " "))
	want := strings.Fields(PreprocessForTTS(title, content))
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("segment words diverge from PreprocessForTTS:\n got %v\nwant %v", got, want)
	}
	if segs[0].PauseAfterMs != ttsTitlePauseMs {
		t.Errorf("title pause = %d, want %d", segs[0].PauseAfterMs, ttsTitlePauseMs)
	}
	if segs[1].PauseAfterMs != ttsParagraphPauseMs {
		t.Errorf("paragraph pause = %d, want %d", segs[1].PauseAfterMs, ttsParagraphPauseMs)
	}
	if segs[len(segs)-1].PauseAfterMs != 0 {
		t.Errorf("last segment pause = %d, want 0", segs[len(segs)-1].PauseAfterMs)
	}
}

// When the content opens with its own title, no title segment is spoken and
// the first paragraph gets the ordinary paragraph pause.
func TestPreprocessSegmentsNoTitleDuplication(t *testing.T) {
	title := "Chapter 1"
	content := "Chapter 1\n\nCall me Ishmael.\n\nSome years ago."
	segs := PreprocessForTTSSegments(title, content)
	for _, s := range segs {
		if s.PauseAfterMs == ttsTitlePauseMs {
			t.Errorf("no segment should carry the title pause when the title is not spoken separately: %+v", segs)
		}
	}
}

// `\b\w` treats an apostrophe as a boundary, so all-caps headings came out
// "Marley'S Ghost." — the second line a Carol listener hears. Contraction and
// possessive suffixes fold back to lower; a real capital after an apostrophe
// (O'Brien) is kept. Both apostrophe shapes.
func TestToTitleCaseApostrophes(t *testing.T) {
	cases := map[string]string{
		"MARLEY'S GHOST.":      "Marley's Ghost.",
		"MARLEY’S GHOST.":      "Marley’s Ghost.",
		"DON'T LOOK BACK":      "Don't Look Back",
		"WE'LL MEET AGAIN":     "We'll Meet Again",
		"THEY'RE HERE":         "They're Here",
		"I'VE SEEN IT":         "I've Seen It",
		"SHE'D KNOW":           "She'd Know",
		"I'M HERE":             "I'm Here",
		"O'BRIEN'S RETURN":     "O'Brien's Return",
		"STAVE ONE. THE FIRST": "Stave One. The First",
	}
	for in, want := range cases {
		if got := toTitleCase(in); got != want {
			t.Errorf("toTitleCase(%q) = %q, want %q", in, got, want)
		}
	}
}
