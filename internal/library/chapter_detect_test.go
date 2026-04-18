package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// synth builds a fake word stream. "||" marks a silence gap (start - prev.end = 3s).
// Otherwise words are 0.5s apart back-to-back.
func synth(script string) []db.SyncTimestamp {
	var words []db.SyncTimestamp
	t := 0.0
	for _, token := range strings.Fields(script) {
		if token == "||" {
			t += 3.0
			continue
		}
		words = append(words, db.SyncTimestamp{Start: t, End: t + 0.3, Word: token})
		t += 0.5
	}
	return words
}

func TestDetectChapters_BasicSequence(t *testing.T) {
	w := synth("once upon a time || Chapter one the beginning of our tale " +
		"many words later || Chapter two things got worse " +
		"even more words || Chapter three the end is near")
	got := DetectChapters(w, 1000)
	if len(got) != 3 {
		t.Fatalf("want 3 chapters, got %d: %+v", len(got), got)
	}
	for i, want := range []int{1, 2, 3} {
		if got[i].Number != want {
			t.Errorf("chapter %d: number=%d want %d", i, got[i].Number, want)
		}
	}
	if got[0].StartSec >= got[1].StartSec {
		t.Error("chapters not in ascending time order")
	}
	// Each got silence + sequence boost.
	if got[0].Confidence < 0.9 {
		t.Errorf("chapter 1 confidence=%v, expected near 1.0 (base+silence+sequence)", got[0].Confidence)
	}
}

func TestDetectChapters_RejectsOrphan(t *testing.T) {
	// "Chapter seventeen" appears in dialogue with no 16 before or 18 after.
	// It should be rejected — only 1 and 2 form a valid sequence.
	w := synth("|| Chapter one begins here " +
		"she said chapter seventeen was her favorite " +
		"|| Chapter two continues")
	got := DetectChapters(w, 1000)
	if len(got) != 2 {
		t.Fatalf("want 2 chapters, got %d: %+v", len(got), got)
	}
	if got[0].Number != 1 || got[1].Number != 2 {
		t.Errorf("numbers: %d, %d", got[0].Number, got[1].Number)
	}
}

func TestDetectChapters_RejectsSingleMatch(t *testing.T) {
	// One match alone isn't a sequence — probably narration flavor.
	w := synth("The story begins here once upon a time chapter one was amazing")
	got := DetectChapters(w, 1000)
	if len(got) != 0 {
		t.Errorf("want 0 (single match is not a sequence), got %d: %+v", len(got), got)
	}
}

func TestDetectChapters_Digits(t *testing.T) {
	w := synth("|| Chapter 1 begins || Chapter 2 middle || Chapter 3 end")
	got := DetectChapters(w, 1000)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
}

func TestDetectChapters_CompoundNumbers(t *testing.T) {
	w := synth("|| Chapter twenty one || Chapter twenty two || Chapter twenty three")
	got := DetectChapters(w, 1000)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	if got[0].Number != 21 || got[1].Number != 22 || got[2].Number != 23 {
		t.Errorf("compound parse wrong: %d %d %d", got[0].Number, got[1].Number, got[2].Number)
	}
}

func TestDetectChapters_PartsWinWhenLonger(t *testing.T) {
	// Real "Part" sequence (4 items) beats a coincidental chapter mention.
	w := synth("|| Part one begins || chapter one was his life's work " +
		"|| Part two continues || Part three develops || Part four ends")
	got := DetectChapters(w, 1000)
	if len(got) != 4 {
		t.Fatalf("want 4 parts, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "part" {
		t.Errorf("kind=%q, want part", got[0].Kind)
	}
}

func TestDetectChapters_EndSecTerminalChapter(t *testing.T) {
	w := synth("|| Chapter one begins || Chapter two ends")
	got := DetectChapters(w, 500.0)
	if got[len(got)-1].EndSec != 500.0 {
		t.Errorf("last chapter EndSec=%v, want 500.0 (audio duration)", got[len(got)-1].EndSec)
	}
	if got[0].EndSec != got[1].StartSec {
		t.Errorf("chapter 1 EndSec should equal chapter 2 StartSec")
	}
}

func TestDetectChapters_Empty(t *testing.T) {
	if got := DetectChapters(nil, 100); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
}

func TestDetectChapters_NoChapterLanguage(t *testing.T) {
	w := synth("the quick brown fox jumps over the lazy dog " +
		"nothing here about chapters or parts at all really")
	got := DetectChapters(w, 100)
	if len(got) != 0 {
		t.Errorf("no chapter language should yield 0 chapters, got %d", len(got))
	}
}
