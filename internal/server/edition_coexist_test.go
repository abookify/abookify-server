package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// The per-chapter regenerate used to take a legacy path — flat chunks, no
// pauses, no content-addressing, a voice-less tts-book-<id>/ dir — and
// register the result as a NEW one-chapter audio source on the work, so the
// regen button quietly grew stray editions (found 2026-09-22 while verifying
// the GPU switch with a real regenerate). It must land in the edition's own
// dir and update that row in place: same number of Kokoro rows before and
// after, same edition label, nothing under a voice-less dir.
func TestRegenerateChapter_StaysInsideItsEdition(t *testing.T) {
	srv, store, dir := newTestServer(t)
	workID, textBookID := seedTextChapters(t, store, 2)
	srv.Generator = library.NewGenerator(store, &fakeTTS{}, nil, dir, srv.OnJobUpdate)

	waitJob := func(jobID string) *db.Job {
		for i := 0; i < 100; i++ {
			if j, _ := store.GetJob(jobID); j != nil && (j.Status == "completed" || j.Status == "failed") {
				return j
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("job %s never reached a terminal state", jobID)
		return nil
	}
	jobID, started := srv.Generator.GenerateAudioFromText(workID, textBookID, "af_heart", "Kokoro · Heart")
	if !started {
		t.Fatal("generate job did not start")
	}
	if j := waitJob(jobID); j.Status != "completed" {
		t.Fatalf("generate: %s (%s)", j.Status, j.Error)
	}
	kokoroRows := func() (n int, editions map[string]int, paths []string) {
		editions = map[string]int{}
		books, _ := store.ListBooks()
		for _, b := range books {
			if b.WorkID == workID && b.Origin == "tts_kokoro" {
				n++
				editions[b.Edition]++
				paths = append(paths, b.Path)
			}
		}
		return
	}
	nBefore, edBefore, _ := kokoroRows()
	if nBefore != 2 {
		t.Fatalf("expected 2 Kokoro chapter rows after generate, got %d", nBefore)
	}

	ch, err := store.GetChapterContent(textBookID, 1)
	if err != nil || ch == nil {
		t.Fatalf("chapter 1: %v", err)
	}
	regenID, started := srv.Generator.RegenerateChapter(workID, textBookID, ch, "af_heart")
	if !started {
		t.Fatal("regenerate job did not start")
	}
	if j := waitJob(regenID); j.Status != "completed" {
		t.Fatalf("regenerate: %s (%s)", j.Status, j.Error)
	}

	nAfter, edAfter, paths := kokoroRows()
	if nAfter != nBefore {
		t.Errorf("Kokoro rows went %d → %d: regenerate created a new audio source instead of updating the chapter in place\n%v", nBefore, nAfter, paths)
	}
	if edAfter["Kokoro · Heart"] != edBefore["Kokoro · Heart"] || edAfter[""] != 0 {
		t.Errorf("edition labels changed: before %v, after %v", edBefore, edAfter)
	}
	for _, p := range paths {
		if !strings.Contains(p, "tts-book-"+strconv.FormatInt(textBookID, 10)+"-af-heart"+string(filepath.Separator)) {
			t.Errorf("chapter landed outside its edition dir: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "tts-book-"+strconv.FormatInt(textBookID, 10))); err == nil {
		t.Errorf("voice-less legacy dir was created")
	}
}
