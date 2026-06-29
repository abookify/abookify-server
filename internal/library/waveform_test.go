package library

import "testing"

// #180: mergeWaveforms lays each per-file waveform onto one book-global
// timeline weighted by duration, and reports the summed duration + BookID 0.
func TestMergeWaveforms(t *testing.T) {
	mk := func(v float32) *Waveform {
		p := make([]float32, waveformPeaks)
		for i := range p {
			p[i] = v
		}
		return &Waveform{Peaks: p}
	}
	// Two equal-duration files: a loud one then a quiet one.
	wf, err := mergeWaveforms([]*Waveform{mk(1.0), mk(0.4)}, []float64{100, 100})
	if err != nil {
		t.Fatal(err)
	}
	if wf.BookID != 0 {
		t.Errorf("merged BookID = %d, want 0", wf.BookID)
	}
	if wf.Duration != 200 {
		t.Errorf("duration = %v, want 200 (sum)", wf.Duration)
	}
	if len(wf.Peaks) != waveformPeaks {
		t.Fatalf("peaks len = %d, want %d", len(wf.Peaks), waveformPeaks)
	}
	// First half should reflect the loud file, second half the quiet one.
	if got := wf.Peaks[waveformPeaks/4]; got < 0.9 {
		t.Errorf("first-half peak = %v, want ~1.0", got)
	}
	if got := wf.Peaks[waveformPeaks*3/4]; got > 0.6 || got < 0.2 {
		t.Errorf("second-half peak = %v, want ~0.4", got)
	}
	// No holes — every bucket covered.
	for i, p := range wf.Peaks {
		if p == 0 {
			t.Fatalf("bucket %d is a hole", i)
		}
	}
	// Degenerate: zero total duration is an error, not a panic.
	if _, err := mergeWaveforms([]*Waveform{mk(1)}, []float64{0}); err == nil {
		t.Error("zero total duration should error")
	}
}
