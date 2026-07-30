package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A coherent transcript work — chunks re-derive exactly, sync count matches, trust
// count matches — must report coherent; then corrupting ONE surface at a time must
// flip it incoherent and name that surface. This is the failure mode the check
// exists for: a repaired book where the reader text and a derived surface disagree.
func TestWorkCoherence(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	workID, err := store.CreateWork("Coherence Work", "Author")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/x/a.mp3", Filename: "a.mp3", Format: "mp3", MediaType: "audio", Title: "Audio",
	}); err != nil {
		t.Fatalf("audio: %v", err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID: workID, Path: "/x/t.txt", Filename: "t.txt", Format: "transcript",
		Origin: "whisper_transcript", MediaType: "text", Title: "Transcript",
	}); err != nil {
		t.Fatalf("transcript: %v", err)
	}
	var transID int64
	for _, b := range mustBooks(store) {
		if b.Path == "/x/t.txt" {
			transID = b.ID
		}
	}

	// One content chapter: 6 words.
	content := "the creature was gentle and kind"
	words := strings.Fields(content)
	store.InsertChapter(db.Chapter{BookID: transID, Index: 0, Title: "Ch1", Content: content, WordCount: len(words), StartSec: 0, EndSec: 100})
	// A chunk that re-derives EXACTLY from the chapter's words (coherent).
	store.InsertChunk(db.Chunk{BookID: transID, ChapterIdx: 0, ChunkIdx: 0, Content: strings.Join(words, " "), StartWord: 0, EndWord: len(words)})
	// sync_data whose word count matches the transcript (coherent).
	store.SaveSyncData(workID, transID, 0, `[{"w":"the","s":0,"e":1},{"w":"creature","s":1,"e":2},{"w":"was","s":2,"e":3},{"w":"gentle","s":3,"e":4},{"w":"and","s":4,"e":5},{"w":"kind","s":5,"e":6}]`)
	// text-trust verdict computed over the same 6 words (coherent).
	store.SaveTextTrust(db.TextTrustRow{WorkID: workID, HasConfidence: true, TotalWords: len(words)})

	wc, err := CheckWorkCoherence(store, workID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !wc.Coherent {
		t.Fatalf("baseline should be coherent, got issues: %+v", wc.Issues)
	}

	// (1) Rewrite a chunk's text (the invented-passage case) → incoherent qa_chunks.
	store.InsertChunk(db.Chunk{BookID: transID, ChapterIdx: 0, ChunkIdx: 0, Content: "the MONSTER was gentle and kind", StartWord: 0, EndWord: len(words)})
	assertIncoherent(t, store, workID, "qa_chunks")
	// Restore the chunk.
	store.InsertChunk(db.Chunk{BookID: transID, ChapterIdx: 0, ChunkIdx: 0, Content: strings.Join(words, " "), StartWord: 0, EndWord: len(words)})

	// (2) Break sync (count far off) → incoherent sync.
	store.SaveSyncData(workID, transID, 0, `[{"w":"the","s":0,"e":1}]`) // 1 word vs 6
	assertIncoherent(t, store, workID, "sync")

	// Restore sync, then stale the trust verdict (word count far from the sync
	// stream) → DEGRADED text_trust (a badge lag, not the reader showing wrong text).
	store.SaveSyncData(workID, transID, 0, `[{"w":"the","s":0,"e":1},{"w":"creature","s":1,"e":2},{"w":"was","s":2,"e":3},{"w":"gentle","s":3,"e":4},{"w":"and","s":4,"e":5},{"w":"kind","s":5,"e":6}]`)
	store.SaveTextTrust(db.TextTrustRow{WorkID: workID, HasConfidence: true, TotalWords: 999})
	wc2, err2 := CheckWorkCoherence(store, workID)
	if err2 != nil {
		t.Fatalf("check: %v", err2)
	}
	if !wc2.Coherent {
		t.Fatalf("stale trust should be DEGRADED not incoherent, got incoherent: %+v", wc2.Issues)
	}
	if !wc2.Degraded {
		t.Fatal("stale trust should flag degraded")
	}
	found := false
	for _, iss := range wc2.Issues {
		if iss.Surface == "text_trust" && iss.Severity == coherenceSeverityDegraded {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a degraded text_trust issue, got %+v", wc.Issues)
	}
}

func assertIncoherent(t *testing.T, store *db.Store, workID int64, surface string) {
	t.Helper()
	wc, err := CheckWorkCoherence(store, workID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if wc.Coherent {
		t.Fatalf("expected incoherent on %s, got coherent", surface)
	}
	for _, iss := range wc.Issues {
		if iss.Surface == surface && iss.Severity == coherenceSeverityIncoherent {
			return
		}
	}
	t.Fatalf("expected an incoherent %s issue, got %+v", surface, wc.Issues)
}
