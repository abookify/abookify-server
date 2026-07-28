package server

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestGroupEditions(t *testing.T) {
	// Two LibriVox mp3s (one edition, grouped by album) + a Kokoro TTS edition
	// + an internal pipeline file that must be excluded.
	audio := []db.Book{
		{ID: 1, MediaType: "audio", Format: "mp3", Album: "LibriVox — Jane", Origin: "librivox", Duration: 100, ChapterCount: 1},
		{ID: 2, MediaType: "audio", Format: "mp3", Album: "LibriVox — Jane", Origin: "librivox", Duration: 120, ChapterCount: 1},
		{ID: 3, MediaType: "audio", Format: "mp3", Edition: "Kokoro · Heart", Origin: "tts_kokoro", Duration: 90, ChapterCount: 1},
		{ID: 9, MediaType: "audio", Format: "mp3", Origin: "tts_preprocessed", Visibility: "internal"},
	}
	// default = the Kokoro book (id 3)
	eds := groupEditions(audio, "audio", 3)
	if len(eds) != 2 {
		t.Fatalf("got %d editions, want 2 (internal excluded): %+v", len(eds), eds)
	}
	lib, kok := eds[0], eds[1]
	if lib.FileCount != 2 || len(lib.BookIDs) != 2 {
		t.Errorf("librivox edition should have 2 files, got %d", lib.FileCount)
	}
	if lib.Provenance != "LibriVox (human)" || lib.ProvKind != "human" {
		t.Errorf("librivox provenance = %q/%q", lib.Provenance, lib.ProvKind)
	}
	if lib.DurationSecs != 220 {
		t.Errorf("librivox duration = %v, want 220", lib.DurationSecs)
	}
	if lib.IsDefault {
		t.Error("librivox should not be default")
	}
	if kok.Label != "Kokoro · Heart" || kok.Provenance != "Kokoro TTS (Abookify)" || kok.ProvKind != "tts" {
		t.Errorf("kokoro edition = %q / %q / %q", kok.Label, kok.Provenance, kok.ProvKind)
	}
	if !kok.IsDefault {
		t.Error("kokoro should be the default (id 3)")
	}
}

func TestProvenanceFor(t *testing.T) {
	cases := map[string][2]string{
		"librivox":     {"LibriVox (human)", "human"},
		"tts_kokoro":   {"Kokoro TTS (Abookify)", "tts"},
		"user_upload":  {"Your import", "personal"},
		"":             {"Your import", "personal"},
		"publisher_epub": {"Publisher EPUB", "publisher"},
	}
	for origin, want := range cases {
		l, k := provenanceFor(origin)
		if l != want[0] || k != want[1] {
			t.Errorf("provenanceFor(%q) = %q/%q, want %q/%q", origin, l, k, want[0], want[1])
		}
	}
}
