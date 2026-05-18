// Pipeline snapshot test against a real sidecar excerpt. Runs the full
// import flow (chapter detection → title extraction → title trim →
// paragraph splitting → gap detection) and asserts the resulting DB
// rows against golden values captured from a known-good run.
//
// Why this exists: most of the regressions this codebase has seen
// (paragraph breaks mid-sentence, title bleed-in, words fused across
// paragraphs, schema mismatches) are invisible at the unit-test level
// because the bug only appears when multiple functions disagree. A
// single fixture-based test exercises all of them together and catches
// "the two functions used different silence-boundary rules" before it
// ships.
//
// To regenerate the golden after a deliberate algorithm change: run
//   go test ./internal/library/... -run TestPipelineSnapshot -update
// and review the diff before committing.
package library

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden snapshot from the current pipeline output")

// snapshot captures the user-observable shape of a finished import.
// Word-level timestamps are intentionally NOT included — they shift
// with every Whisper retranscription and would make the snapshot
// noisy. Titles, paragraph layout, and gap detection are the parts a
// user actually sees.
type snapshot struct {
	Chapters []chapterSnapshot `json:"chapters"`
	Gaps     []gapSnapshot     `json:"transcription_gaps"`
}

type chapterSnapshot struct {
	Index          int      `json:"index"`
	Title          string   `json:"title"`
	WordCount      int      `json:"word_count"`
	ParagraphCount int      `json:"paragraph_count"`
	FirstSentence  string   `json:"first_sentence"` // first ~80 chars of paragraph 0
}

type gapSnapshot struct {
	StartSec    float64 `json:"start_sec"`
	EndSec      float64 `json:"end_sec"`
	DurationSec float64 `json:"duration_sec"`
	SourceFile  string  `json:"source_file"`
}

func TestPipelineSnapshot_NormExcerpt(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create a work + audio book to attach the sidecar to. The path on
	// the book row points at a real-looking file path; importOneSidecar
	// doesn't actually read it, just uses the work/book IDs for FKs.
	workID, err := store.CreateWork("Norm Macdonald excerpt", "Norm Macdonald")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBook(db.Book{
		WorkID:    workID,
		Path:      "/library/audiobooks/norm-excerpt.mp3",
		Filename:  "norm-excerpt.mp3",
		Format:    "mp3",
		MediaType: "audio",
		Origin:    "narrator_recording",
	}); err != nil {
		t.Fatal(err)
	}
	books, _ := store.ListBooks()
	var audioBookID int64
	for _, b := range books {
		if b.WorkID == workID && b.MediaType == "audio" {
			audioBookID = b.ID
			break
		}
	}
	if audioBookID == 0 {
		t.Fatal("audio book row not found after upsert")
	}

	fixture := filepath.Join("testdata", "norm_excerpt.stt.json")
	if err := importOneSidecar(store, workID, audioBookID, fixture); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Collect the snapshot from the populated DB.
	got := buildSnapshot(t, store, workID)

	goldenPath := filepath.Join("testdata", "norm_excerpt.golden.json")
	if *updateGolden {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(goldenPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", goldenPath)
		return
	}

	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create)", err)
	}
	gotBytes, _ := json.MarshalIndent(got, "", "  ")
	if string(gotBytes) != strings.TrimSpace(string(wantBytes)) {
		t.Errorf("snapshot mismatch — re-run with -update to inspect/accept.\n--- want\n%s\n--- got\n%s",
			string(wantBytes), string(gotBytes))
	}

	// Per-paragraph invariant: NO paragraph row's text should contain
	// "\n\n" — that means the content builder inserted a paragraph
	// break that the paragraph row splitter didn't pick up, leaving a
	// fused row. This is the exact shape the silence-boundary disagreement
	// bug had earlier this session.
	work, _ := store.GetWork(workID)
	for _, tb := range work.TextFiles {
		chapters, _ := store.ListChapters(tb.ID)
		for _, ch := range chapters {
			paras, _ := store.ListParagraphs(tb.ID, ch.Index)
			for _, p := range paras {
				if strings.Contains(p.Text, "\n\n") {
					t.Errorf("paragraph %d/%d contains \\n\\n — content builder and detector disagree on a silence boundary",
						p.ChapterIdx, p.ParagraphIdx)
				}
			}
		}
	}
}

func buildSnapshot(t *testing.T, store *db.Store, workID int64) snapshot {
	t.Helper()
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		t.Fatalf("get work: %v", err)
	}

	var snap snapshot
	for _, tb := range work.TextFiles {
		if tb.Format != "transcript" {
			continue
		}
		chapters, _ := store.ListChapters(tb.ID)
		for _, ch := range chapters {
			cps, _ := store.ListParagraphs(tb.ID, ch.Index)
			first := ""
			if len(cps) > 0 {
				first = cps[0].Text
				if len(first) > 80 {
					first = first[:80]
				}
				first = strings.ReplaceAll(first, "\n", " ")
			}
			snap.Chapters = append(snap.Chapters, chapterSnapshot{
				Index:          ch.Index,
				Title:          ch.Title,
				WordCount:      ch.WordCount,
				ParagraphCount: len(cps),
				FirstSentence:  first,
			})
		}
	}

	// Transcription gaps live on the audio book row.
	for _, ab := range work.AudioFiles {
		raw, _ := store.GetTranscriptionGaps(ab.ID)
		if raw == "" || raw == "[]" {
			continue
		}
		var gaps []gapSnapshot
		_ = json.Unmarshal([]byte(raw), &gaps)
		snap.Gaps = append(snap.Gaps, gaps...)
	}
	return snap
}
