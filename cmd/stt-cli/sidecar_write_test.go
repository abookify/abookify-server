package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/stt"
)

func sampleResult() *stt.TranscribeResult {
	return &stt.TranscribeResult{
		Language: "en",
		Segments: []stt.Segment{{
			Start: 0, End: 2, Text: "hello there",
			Words: []stt.Word{
				{Word: "hello", Start: 0.1, End: 0.5, Probability: 0.9},
				{Word: "there", Start: 0.6, End: 1.0, Probability: 0.8},
			},
		}},
	}
}

// A checkpoint must describe EXACTLY the files completed so far — that is what
// makes it resumable with --redo-files — while still reporting the full run's
// duration, which the importer's offset mapping depends on.
func TestBuildSidecarPartialCheckpoint(t *testing.T) {
	files := []string{"/a/01.mp3", "/a/02.mp3", "/a/03.mp3"}
	durs := []float64{600, 600, 600}

	doc := buildSidecar(sampleResult(), nil, files[:2], durs[:2], 1800)

	if doc.Version != 3 || doc.Schema != "abookify-sidecar/v3" {
		t.Errorf("version/schema = %d/%q", doc.Version, doc.Schema)
	}
	if len(doc.Sources) != 2 {
		t.Fatalf("sources = %d, want 2 (only completed files)", len(doc.Sources))
	}
	if doc.Sources[0].Filename != "01.mp3" || doc.Sources[1].StartSec != 600 {
		t.Errorf("sources wrong: %+v", doc.Sources)
	}
	if doc.Duration != 1800 {
		t.Errorf("duration = %v, want the FULL 1800 (not the partial sum)", doc.Duration)
	}
	if len(doc.Words) != 2 {
		t.Errorf("words = %d, want 2", len(doc.Words))
	}
}

// A single-file run carries no sources array (nothing to map back to).
func TestBuildSidecarSingleFileHasNoSources(t *testing.T) {
	doc := buildSidecar(sampleResult(), nil, []string{"/a/only.mp3"}, []float64{600}, 600)
	if len(doc.Sources) != 0 {
		t.Errorf("sources = %d, want 0 for a single file", len(doc.Sources))
	}
}

// The write must be atomic. A checkpoint torn by a crash mid-write would leave
// truncated JSON — destroying the very work it exists to protect — so the
// implementation writes a temp file and renames.
func TestWriteSidecarIsAtomicAndValid(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "book.stt.json")

	prior := []byte(`{"version":3,"words":[{"w":"old"}]}`)
	if err := os.WriteFile(out, prior, 0o644); err != nil {
		t.Fatal(err)
	}

	files := []string{"/a/01.mp3", "/a/02.mp3"}
	if err := writeSidecar(out, sampleResult(), nil, files, []float64{600, 600}, 1200); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}

	// Valid JSON of the right shape — not truncated, not the old content.
	var got sidecarDoc
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("sidecar is not valid JSON (a torn write would look like this): %v", err)
	}
	if len(got.Words) != 2 || got.Words[0].Word != "hello" {
		t.Errorf("content not replaced: %+v", got.Words)
	}

	// No temp files left behind on success.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" || len(e.Name()) > 4 && e.Name()[:5] == ".stt-" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(ents) != 1 {
		t.Errorf("expected exactly the sidecar in the dir, got %d entries", len(ents))
	}
}
