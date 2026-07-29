package main

import (
	"strings"
	"testing"
)

func sidecarWith(srcs ...sourceInfo) *sidecarV3 {
	return &sidecarV3{Version: 3, Schema: "abookify-sidecar/v3", Sources: srcs}
}

// Offsets derived from the directory must agree with the sidecar's own sources
// array. When they don't, the redo has to refuse: whisper still transcribes
// correctly, so the failure is invisible — the words are simply written to the
// wrong place and the merge deletes good words from the range it thinks it is
// replacing.
func TestVerifyAgainstSidecar(t *testing.T) {
	good := sidecarWith(
		sourceInfo{Filename: "1.mp3", StartSec: 0, Duration: 100},
		sourceInfo{Filename: "2.mp3", StartSec: 100, Duration: 200},
		sourceInfo{Filename: "3.mp3", StartSec: 300, Duration: 300},
	)

	t.Run("matching layout passes", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/2.mp3", "/a/3.mp3"},
			[]float64{100, 200, 300}, []float64{0, 100, 300}, good)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
	})

	// The Life of Pi case: a scratch file joins the *.mp3 glob and pushes every
	// later file down the timeline.
	t.Run("stray file rejected", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/2.mp3", "/a/2.repair.999.mp3", "/a/3.mp3"},
			[]float64{100, 200, 200, 300}, []float64{0, 100, 300, 500}, good)
		if err == nil {
			t.Fatal("stray file accepted — offsets would silently shift")
		}
		if !strings.Contains(err.Error(), "2.repair.999.mp3") {
			t.Errorf("error does not name the stray file: %v", err)
		}
	})

	t.Run("missing file rejected", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/3.mp3"},
			[]float64{100, 300}, []float64{0, 100}, good)
		if err == nil {
			t.Fatal("missing file accepted")
		}
	})

	t.Run("reordering rejected", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/3.mp3", "/a/2.mp3"},
			[]float64{100, 300, 200}, []float64{0, 100, 400}, good)
		if err == nil {
			t.Fatal("reordered files accepted")
		}
		if !strings.Contains(err.Error(), "3.mp3") {
			t.Errorf("error does not identify the position that diverged: %v", err)
		}
	})

	t.Run("changed duration rejected", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/2.mp3", "/a/3.mp3"},
			[]float64{100, 260, 300}, []float64{0, 100, 360}, good)
		if err == nil {
			t.Fatal("duration change accepted — later files would shift 60s")
		}
	})

	// A repaired file is re-encoded and can move by a frame or two. That must
	// NOT block a redo, or the repair-then-refill workflow cannot run at all.
	t.Run("sub-second drift tolerated", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3", "/a/2.mp3", "/a/3.mp3"},
			[]float64{100.08, 199.94, 300.02}, []float64{0, 100.08, 300.02}, good)
		if err != nil {
			t.Fatalf("frame-level drift from re-encoding rejected: %v", err)
		}
	})

	// Old sidecars predate the sources array. Warn, don't crash, don't block.
	t.Run("no sources array warns but proceeds", func(t *testing.T) {
		err := verifyAgainstSidecar(
			[]string{"/a/1.mp3"}, []float64{100}, []float64{0}, sidecarWith())
		if err != nil {
			t.Fatalf("sourceless sidecar blocked the redo: %v", err)
		}
	})
}
