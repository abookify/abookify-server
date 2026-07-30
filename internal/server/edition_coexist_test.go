package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

// The heart of multi-edition management: generating a TTS edition must ADD a new
// edition alongside the existing sources, never clobber them — so a reader's
// LibriVox narration, our Kokoro edition, and their own text copy sit side by
// side on one work (PJ's stated want). This drives the real generate path
// (GenerateAudioFromText → runTTS → per-chapter UpsertBook with an Edition
// label) with a fake TTS provider and asserts coexistence.
func TestGenerateEdition_CoexistsWithExistingSources(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID, textBookID := seedTextChapters(t, store, 2)

	// An existing narration edition already on the work (his LibriVox copy).
	narrationPath := filepath.Join(dir, "librivox_ch1.mp3")
	if err := os.WriteFile(narrationPath, []byte("existing narration"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: narrationPath, Filename: "librivox_ch1.mp3",
		Format: "mp3", MediaType: "audio", Title: "Chapter 1",
		Origin: "narrator_recording", Edition: "LibriVox",
	}); err != nil {
		t.Fatal(err)
	}

	// Generate a Kokoro edition from the text (fake TTS, no STT → no alignment).
	srv.Generator = library.NewGenerator(store, &fakeTTS{}, nil, dir, srv.OnJobUpdate)
	jobID, started := srv.Generator.GenerateAudioFromText(workID, textBookID, "af_heart", "Kokoro · Heart")
	if !started {
		t.Fatalf("generate job did not start")
	}

	// The job runs on the single background worker — wait for it.
	var job *db.Job
	for i := 0; i < 100; i++ {
		j, _ := store.GetJob(jobID)
		if j != nil && (j.Status == "completed" || j.Status == "failed") {
			job = j
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if job == nil {
		t.Fatal("tts job never reached a terminal state")
	}
	if job.Status != "completed" {
		t.Fatalf("tts job status = %q (err: %s), want completed", job.Status, job.Error)
	}

	// Coexistence: original text + original narration preserved, new edition added.
	books, err := store.ListBooks()
	if err != nil {
		t.Fatalf("list books: %v", err)
	}
	var text, librivox, kokoro int
	for _, b := range books {
		if b.WorkID != workID {
			continue
		}
		switch {
		case b.MediaType == "text":
			text++
		case b.Edition == "LibriVox":
			librivox++
		case b.Edition == "Kokoro · Heart" && b.Origin == "tts_kokoro":
			kokoro++
		}
	}
	if text != 1 {
		t.Errorf("text editions = %d, want 1 (original text preserved)", text)
	}
	if librivox != 1 {
		t.Errorf("LibriVox narration = %d, want 1 (must NOT be clobbered by the new edition)", librivox)
	}
	if kokoro < 1 {
		t.Errorf("Kokoro edition books = %d, want >=1 (the new coexisting edition)", kokoro)
	}
}
