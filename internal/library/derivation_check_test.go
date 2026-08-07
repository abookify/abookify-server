package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestCollapsedToOneIndex(t *testing.T) {
	cases := []struct {
		name string
		idx  []int
		want bool
	}{
		{"empty", nil, false},
		{"single", []int{3}, false},
		{"distinct", []int{0, 4, 9, 17, 25}, false},  // work 76 after the fix
		{"collapsed", []int{8, 8, 8, 8, 8}, true},    // work 76 before the fix
		{"one-outlier", []int{8, 8, 8, 9, 8}, false}, // not fully collapsed
	}
	for _, c := range cases {
		var links []db.ChapterLink
		for _, i := range c.idx {
			links = append(links, db.ChapterLink{TextIndex: i})
		}
		if got := collapsedToOneIndex(links); got != c.want {
			t.Errorf("%s: collapsedToOneIndex=%v want %v", c.name, got, c.want)
		}
	}
}

func TestNarrationEnd(t *testing.T) {
	// Work 86 shape: a 7-file LibriVox chain and a single-file AI reading, BOTH
	// starting at 0. The chain walk must separate them so neither narration's end
	// bleeds into the other (the false positive the per-narration model fixes).
	files := []db.Book{
		{ID: 1, StartSec: 0, Duration: 1507},     // LibriVox 1
		{ID: 2, StartSec: 0, Duration: 10795},    // AI (single file)
		{ID: 3, StartSec: 1507, Duration: 1361},  // LibriVox 2
		{ID: 4, StartSec: 2868, Duration: 2066},  // LibriVox 3
		{ID: 5, StartSec: 4934, Duration: 1331},  // LibriVox 4
		{ID: 6, StartSec: 6265, Duration: 2159},  // LibriVox 5
		{ID: 7, StartSec: 8424, Duration: 1924},  // LibriVox 6
		{ID: 8, StartSec: 10348, Duration: 2498}, // LibriVox 7
	}
	if got := narrationEnd(files, 1); got < 12844 || got > 12848 {
		t.Errorf("LibriVox narration end = %.0f, want ~12846", got)
	}
	if got := narrationEnd(files, 2); got != 10795 {
		t.Errorf("AI narration end = %.0f, want 10795 (must not chain into LibriVox)", got)
	}
	if got := narrationEnd(files, 99); got != 0 {
		t.Errorf("unknown rep book = %.0f, want 0", got)
	}

	// Single-narration chain (work 76 shape): 5 files chaining to ~17808.
	w76 := []db.Book{
		{ID: 10, StartSec: 0, Duration: 3022},
		{ID: 11, StartSec: 3021, Duration: 3794},
		{ID: 12, StartSec: 6816, Duration: 3380},
		{ID: 13, StartSec: 10196, Duration: 3517},
		{ID: 14, StartSec: 13713, Duration: 4095},
	}
	if got := narrationEnd(w76, 10); got < 17806 || got > 17810 {
		t.Errorf("work-76 narration end = %.0f, want ~17808", got)
	}
}

func TestVerifyDerivation_LinkIntegrity(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	work := seedWork(t, store, 5, 0) // 5 text chapters, no detected audio chapters
	textID := work.TextFiles[0].ID
	audioID := work.AudioFiles[0].ID

	// A single in-range link → passes (no word alignment, so sync isn't asserted).
	store.InsertChapterLink(work.ID, db.ChapterLink{AudioBookID: audioID, AudioIndex: 0, TextBookID: textID, TextIndex: 2, Confidence: 0.9})
	fresh, _ := store.GetWork(work.ID)
	if rep, err := VerifyWorkDerivation(store, fresh); err != nil || !rep.OK {
		t.Fatalf("valid link should pass: err=%v issues=%+v", err, rep.Issues)
	}

	// text_index beyond the 5 chapters → link_out_of_range (never legitimate).
	store.InsertChapterLink(work.ID, db.ChapterLink{AudioBookID: audioID, AudioIndex: 1, TextBookID: textID, TextIndex: 99, Confidence: 0.9})
	fresh, _ = store.GetWork(work.ID)
	rep, _ := VerifyWorkDerivation(store, fresh)
	if rep.OK || !hasIssue(rep, "link_out_of_range") {
		t.Fatalf("out-of-range text_index should fail with link_out_of_range: %+v", rep.Issues)
	}

	// A link pointing at a text book not on this work → link_dangling.
	store.DeleteChapterLinksByWork(work.ID)
	store.InsertChapterLink(work.ID, db.ChapterLink{AudioBookID: audioID, AudioIndex: 0, TextBookID: 999999, TextIndex: 0, Confidence: 0.9})
	fresh, _ = store.GetWork(work.ID)
	rep, _ = VerifyWorkDerivation(store, fresh)
	if rep.OK || !hasIssue(rep, "link_dangling") {
		t.Fatalf("link to a foreign text book should fail with link_dangling: %+v", rep.Issues)
	}
}

func hasIssue(rep DerivationReport, kind string) bool {
	for _, i := range rep.Issues {
		if i.Kind == kind {
			return true
		}
	}
	return false
}

func TestNarrationChainExcludesJunk(t *testing.T) {
	// Work 71 shape: a real 01→03.mp3 chain (0→9600) plus zero-duration junk
	// files (18/19.mp3, start 0 dur 0). The chain must contain ONLY the real
	// files so alignment-derived linking never smears junk across the timeline.
	files := []db.Book{
		{ID: 100, StartSec: 0, Duration: 3600},    // 01.mp3 (chain anchor)
		{ID: 201, StartSec: 0, Duration: 0},       // 18.mp3 junk
		{ID: 202, StartSec: 0, Duration: 0},       // 19.mp3 junk
		{ID: 101, StartSec: 3600, Duration: 3600}, // 02.mp3
		{ID: 102, StartSec: 7200, Duration: 2400}, // 03.mp3
	}
	members, end := narrationChain(files, 100)
	if end < 9598 || end > 9602 {
		t.Errorf("chain end = %.0f, want ~9600", end)
	}
	for _, want := range []int64{100, 101, 102} {
		if !members[want] {
			t.Errorf("chain missing real file %d", want)
		}
	}
	for _, junk := range []int64{201, 202} {
		if members[junk] {
			t.Errorf("chain wrongly included zero-duration junk file %d", junk)
		}
	}
}
