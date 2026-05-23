package main

import "testing"

func TestMergeWords_DropsRangeKeepsRest(t *testing.T) {
	existing := []wordTS{
		{Start: 0, End: 1, Word: "a"},
		{Start: 10, End: 11, Word: "b"}, // in drop range
		{Start: 20, End: 21, Word: "c"},
	}
	fresh := []wordTS{
		{Start: 12, End: 13, Word: "B1"},
		{Start: 15, End: 16, Word: "B2"},
	}
	drop := []timeRange{{start: 5, end: 18}}
	merged := mergeWords(existing, fresh, drop)
	want := []string{"a", "B1", "B2", "c"}
	if len(merged) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(merged), len(want), merged)
	}
	for i, w := range merged {
		if w.Word != want[i] {
			t.Errorf("merged[%d].Word=%q want %q", i, w.Word, want[i])
		}
	}
}

func TestMergeSilences_DropsRangeKeepsRest(t *testing.T) {
	existing := []silenceEvent{
		{Start: 0, End: 1, Kind: "paragraph"},
		{Start: 10, End: 11, Kind: "paragraph"}, // dropped
		{Start: 25, End: 26, Kind: "chapter"},
	}
	fresh := []silenceEvent{
		{Start: 12, End: 13, Kind: "paragraph"},
	}
	drop := []timeRange{{start: 5, end: 20}}
	merged := mergeSilences(existing, fresh, drop)
	if len(merged) != 3 {
		t.Fatalf("len=%d, want 3: %+v", len(merged), merged)
	}
	if merged[0].Start != 0 || merged[1].Start != 12 || merged[2].Start != 25 {
		t.Errorf("merged order wrong: %+v", merged)
	}
}

func TestInAnyRange(t *testing.T) {
	r := []timeRange{{start: 5, end: 10}, {start: 20, end: 30}}
	for _, c := range []struct {
		t      float64
		expect bool
	}{
		{0, false},
		{5, true},   // boundary start: inclusive
		{9.99, true},
		{10, false}, // boundary end: exclusive
		{15, false},
		{25, true},
		{30, false},
	} {
		if got := inAnyRange(c.t, r); got != c.expect {
			t.Errorf("inAnyRange(%v)=%v, want %v", c.t, got, c.expect)
		}
	}
}

func TestWriteBootstrapSidecar_ProducesReadableV3(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/audio.stt.json"
	files := []string{
		"/some/path/chapter-007.mp3",
		"/some/path/chapter-008.mp3",
		"/some/path/chapter-009.mp3",
	}
	durations := []float64{600.5, 720.25, 480.0}
	total := 600.5 + 720.25 + 480.0
	if err := writeBootstrapSidecar(out, files, durations, total); err != nil {
		t.Fatalf("writeBootstrapSidecar: %v", err)
	}
	// readSidecar is the same parser the --redo-files path uses, so a stub
	// that round-trips here is guaranteed schema-compatible with the redo
	// merge logic.
	sc, err := readSidecar(out)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if sc.Version != 3 {
		t.Errorf("Version=%d want 3", sc.Version)
	}
	if sc.Schema != "abookify-sidecar/v3" {
		t.Errorf("Schema=%q", sc.Schema)
	}
	if sc.Duration != total {
		t.Errorf("Duration=%v want %v", sc.Duration, total)
	}
	if len(sc.Sources) != 3 {
		t.Fatalf("len(Sources)=%d want 3", len(sc.Sources))
	}
	if sc.Sources[0].StartSec != 0 {
		t.Errorf("Sources[0].StartSec=%v want 0", sc.Sources[0].StartSec)
	}
	if sc.Sources[1].StartSec != 600.5 {
		t.Errorf("Sources[1].StartSec=%v want 600.5", sc.Sources[1].StartSec)
	}
	if sc.Sources[2].StartSec != 600.5+720.25 {
		t.Errorf("Sources[2].StartSec=%v", sc.Sources[2].StartSec)
	}
	if sc.Sources[0].Filename != "chapter-007.mp3" {
		t.Errorf("Sources[0].Filename=%q", sc.Sources[0].Filename)
	}
	if len(sc.Words) != 0 {
		t.Errorf("stub should have no words, got %d", len(sc.Words))
	}
}
