package library

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// syntheticWhisper builds whisper words at a steady pace: word i spans
// [i*0.4, i*0.4+0.35].
func syntheticWhisper(words []string) []db.SyncTimestamp {
	out := make([]db.SyncTimestamp, len(words))
	for i, w := range words {
		out[i] = db.SyncTimestamp{Start: float64(i) * 0.4, End: float64(i)*0.4 + 0.35, Word: w}
	}
	return out
}

func distinctWords(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// The greedy mapper this test buried carried a 10-word lookahead: a divergence
// longer than that derailed it permanently and every later word piled up at
// prev+0.15s — Alice ch3 stored all 1,702 words ending at 294s of a 554s
// narration despite a COMPLETE whisper transcription. The anchor mapper must
// re-synchronize after the divergence and keep the tail at its real times.
func TestAlignTimestampsToSourceResynchronizesAfterLongDivergence(t *testing.T) {
	head := distinctWords("alpha", 60)
	tail := distinctWords("omega", 60)
	orig := strings.Join(append(append([]string{}, head...), tail...), " ")

	// Whisper heard the head, then 25 inserted words (a spelled-out number,
	// an ad-lib — anything >10 breaks the old lookahead), then the tail.
	junk := distinctWords("junk", 25)
	heard := append(append(append([]string{}, head...), junk...), tail...)
	ww := syntheticWhisper(heard)

	got := AlignTimestampsToSource(orig, ww)
	if len(got) != 120 {
		t.Fatalf("mapped %d words, want 120", len(got))
	}
	// The last original word must sit at the END of the heard timeline, not
	// piled shortly after the divergence.
	wantEnd := ww[len(ww)-1].End
	if gotEnd := got[len(got)-1].End; gotEnd < 0.9*wantEnd {
		t.Errorf("tail collapsed: last mapped word ends %.1fs, whisper timeline ends %.1fs — "+
			"the mapper derailed at the divergence", gotEnd, wantEnd)
	}
	// And the first tail word must start where whisper actually said it
	// (after the junk), not interpolated near the head.
	tailStart := ww[60+25].Start
	if got[60].Start < tailStart-1 {
		t.Errorf("first tail word at %.1fs, want ~%.1fs", got[60].Start, tailStart)
	}
}

// Words whisper never said (divergent ebook-only stretch) must spread linearly
// across the real gap, not stack at its start.
func TestAlignTimestampsToSourceInterpolatesGapsLinearly(t *testing.T) {
	head := distinctWords("alpha", 30)
	gap := distinctWords("silent", 10) // in the text, never spoken
	tail := distinctWords("omega", 30)
	orig := strings.Join(append(append(append([]string{}, head...), gap...), tail...), " ")
	heard := append(append([]string{}, head...), tail...)
	ww := syntheticWhisper(heard)

	got := AlignTimestampsToSource(orig, ww)
	if len(got) != 70 {
		t.Fatalf("mapped %d words, want 70", len(got))
	}
	// The 10 unspoken words sit between head-end and tail-start, increasing.
	t0, t1 := ww[29].End, ww[30].Start
	prev := t0 - 0.001
	for k := 30; k < 40; k++ {
		w := got[k]
		if w.Start < t0-0.001 || w.End > t1+0.001 {
			t.Errorf("gap word %d at [%.2f,%.2f] outside its real gap [%.2f,%.2f]", k, w.Start, w.End, t0, t1)
		}
		if w.Start < prev {
			t.Errorf("gap word %d starts before its predecessor", k)
		}
		prev = w.Start
	}
}
