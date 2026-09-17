package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Whisper words carry their own leading space; one that arrives without it
// ("-off") is glued onto its predecessor by joinWords, so the transcript
// content has ONE whitespace word where the timeline has TWO. The
// content-Fields map drifts by one at every glue (1,160 words by chapter 13 of
// The Selfish Gene — every EPUB word baked minutes early). Tokenizing the
// timeline's own words reproduces the anchor stream exactly.
func TestBuildTokToTimeline_GluedWordsStayExact(t *testing.T) {
	tl := []db.SyncTimestamp{
		{Word: " the", Start: 0, End: 0.5},
		{Word: " showing", Start: 1, End: 1.5},
		{Word: "-off", Start: 2, End: 2.5}, // no leading space: glued in content
		{Word: " power", Start: 3, End: 3.5},
		{Word: " so", Start: 4, End: 4.5},
	}
	content := []ChapterText{{Index: 0, Text: joinWords(tl)}}
	toks, _ := AssembleStream(content)
	if len(toks) != 5 {
		t.Fatalf("token stream = %v, want 5 tokens", toks)
	}

	// The old basis drifts: content Fields = [the showing-off power so] = 4
	// words, so token "so" (index 4) lands on Fields index 3.
	old := buildTokToFields(content)
	if old[4] != 3 {
		t.Fatalf("expected the content-Fields map to drift (got %v)", old)
	}

	m, share := buildTokToTimeline(tl, toks)
	if m == nil || share != 1 {
		t.Fatalf("timeline map refused an exact stream (share %.2f)", share)
	}
	want := []int{0, 1, 2, 3, 4}
	for i := range want {
		if m[i] != want[i] {
			t.Fatalf("tokToTimeline = %v, want %v", m, want)
		}
	}
	segs := []Segment{{EbookStart: 0, EbookEnd: 1, TransStart: 4, TransEnd: 5, Kind: SegAligned}}
	bakeSegmentTimes(segs, tl, m, true)
	if segs[0].StartSec != 4 {
		t.Fatalf("token 'so' baked at %v, want 4 (the timeline word's own start)", segs[0].StartSec)
	}
}

// A timeline that does not reproduce the stream (another edition's narration,
// or content that dropped words) must be refused so the caller falls back.
func TestPickTimelineByTokens_RefusesMismatch(t *testing.T) {
	stream := []string{"one", "two", "three", "four", "five"}
	other := []db.SyncTimestamp{{Word: " a"}, {Word: " different"}, {Word: " recording"}}
	if tl, m := pickTimelineByTokens([][]db.SyncTimestamp{other}, stream); tl != nil || m != nil {
		t.Fatal("a different recording's timeline was accepted")
	}
	exact := []db.SyncTimestamp{{Word: " one"}, {Word: " two"}, {Word: "-three"}, {Word: " four"}, {Word: " five"}}
	tl, m := pickTimelineByTokens([][]db.SyncTimestamp{other, exact}, stream)
	if tl == nil || len(m) != 5 || m[2] != 2 {
		t.Fatalf("exact timeline not picked: tl=%v m=%v", tl != nil, m)
	}
}

// Older imports normalised the content separately from the timeline: the
// content's "o'clock" is one token where the timeline's "o’clock" arrives as
// two, and one content dropped a leading "Section". The walk must resync and
// keep every later token on its own timeline word instead of refusing the
// timeline (which would fall back to the drifting content-Fields map).
func TestBuildTokToTimeline_ResyncsAcrossLocalDivergence(t *testing.T) {
	tl := []db.SyncTimestamp{
		{Word: " Section", Start: 0}, {Word: " one", Start: 1}, {Word: " of", Start: 2}, {Word: " the", Start: 3},
		{Word: " book", Start: 4}, {Word: " at", Start: 5}, {Word: " o", Start: 6}, {Word: "'clock", Start: 6.5},
		{Word: " the", Start: 7}, {Word: " mist", Start: 8}, {Word: " cleared", Start: 9}, {Word: " away", Start: 10},
	}
	stream := []string{"one", "of", "the", "book", "at", "o'clock", "the", "mist", "cleared", "away"}
	m, share := buildTokToTimeline(tl, stream)
	if m == nil || share < 0.85 {
		t.Fatalf("map refused: share %.2f", share)
	}
	// "one" is timeline word 1 (the dropped "Section" is skipped), "o'clock" parks
	// on timeline word 6, and "away" is timeline word 11.
	if m[0] != 1 || m[5] != 6 || m[9] != 11 {
		t.Fatalf("map = %v", m)
	}
	for i := 1; i < len(m); i++ {
		if m[i] < m[i-1] {
			t.Fatalf("map not monotone at %d: %v", i, m)
		}
	}
}
