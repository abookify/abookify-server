package library

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// Card 39: a narration whose detected rows begin at 2000 s, while the
// alignment places two ebook chapters before that, gains those two rows on
// the audio anchor and on the transcript (the lump replaced, content sliced
// from the word timeline); the existing row keeps its title and moves down.
func TestFillLeadingRows_TilesTheLead(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	workID, err := store.CreateWork("Lead Book", "Someone")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(b db.Book) int64 {
		t.Helper()
		b.WorkID = workID
		if err := store.UpsertBook(b); err != nil {
			t.Fatal(err)
		}
		books, _ := store.ListBooks()
		for _, x := range books {
			if x.Path == b.Path {
				return x.ID
			}
		}
		t.Fatalf("book %s not found", b.Path)
		return 0
	}
	anchor := mk(db.Book{Path: "/x/01.mp3", Filename: "01.mp3", Format: "mp3", MediaType: "audio", Origin: "narrator_recording", StartSec: 0, Duration: 1500})
	mk(db.Book{Path: "/x/02.mp3", Filename: "02.mp3", Format: "mp3", MediaType: "audio", Origin: "narrator_recording", StartSec: 1500, Duration: 1500})
	ebook := mk(db.Book{Path: "/x/b.epub", Filename: "b.epub", Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	trans := mk(db.Book{Path: "generated://transcript/1", Filename: "transcript", Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})

	// Ebook: three chapters of 400 words; the alignment's spans match them.
	words := func(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }
	for i, title := range []string{"Chapter I. Dawn", "Chapter II. Noon", "Chapter III. Dusk"} {
		store.InsertChapter(db.Chapter{BookID: ebook, Index: i, Title: title, Content: words(400), WordCount: 400})
	}
	// Narration rows: the detector accepted only the third chapter's announcement.
	store.InsertChapter(db.Chapter{BookID: anchor, Index: 0, Title: "Chapter 3", Src: "detected", StartSec: 2000, EndSec: 3000, Confidence: 0.9})
	store.InsertChapter(db.Chapter{BookID: trans, Index: 0, Title: "Prelude", Src: "detected", Content: "lump", WordCount: 1, StartSec: 0, EndSec: 2000})
	store.InsertChapter(db.Chapter{BookID: trans, Index: 1, Title: "Chapter 3", Src: "detected", Content: "tail", WordCount: 1, StartSec: 2000, EndSec: 3000})
	// Word timeline on the anchor: one word per second, book-continuous.
	var ts []db.SyncTimestamp
	for s := 0; s < 3000; s++ {
		ts = append(ts, db.SyncTimestamp{Start: float64(s), End: float64(s) + 0.5, Word: fmt.Sprintf("w%d", s)})
	}
	raw, _ := json.Marshal(ts)
	if err := store.SaveSyncData(workID, anchor, 0, string(raw)); err != nil {
		t.Fatal(err)
	}
	// Alignment ebook↔transcript: chapter I narrated 5–990 s, II 1000–1990, III 2000–2990.
	payload := AnchorAlignmentPayload{
		Unit:          "word",
		EbookWords:    1200,
		EbookChapters: []ChapterSpan{{Index: 0, Start: 0, Len: 400}, {Index: 1, Start: 400, Len: 400}, {Index: 2, Start: 800, Len: 400}},
		Timeline: []ChapterTimeline{
			{EbookChapterIdx: 0, Unit: "word", Points: []TimelinePoint{{W: 0, Sec: 5}, {W: 399, Sec: 990}}},
			{EbookChapterIdx: 1, Unit: "word", Points: []TimelinePoint{{W: 0, Sec: 1000}, {W: 399, Sec: 1990}}},
			{EbookChapterIdx: 2, Unit: "word", Points: []TimelinePoint{{W: 0, Sec: 2000}, {W: 399, Sec: 2990}}},
		},
	}
	pairs, _ := json.Marshal(payload)
	if err := store.SaveAlignment(db.Alignment{WorkID: workID, FromBookID: ebook, ToBookID: trans, Unit: "word", Confidence: 0.95, Method: "anchor", Pairs: string(pairs)}); err != nil {
		t.Fatal(err)
	}
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		t.Fatalf("work: %v", err)
	}

	dry, err := FillLeadingRows(store, work, ebook, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Applied || len(dry.Added) != 2 || dry.Skipped != "" {
		t.Fatalf("dry run: want 2 rows to add and nothing written, got %+v", dry)
	}
	if rows, _ := store.ListChapters(anchor); len(rows) != 1 {
		t.Fatalf("dry run must not write: anchor has %d rows", len(rows))
	}

	rep, err := FillLeadingRows(store, work, ebook, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied {
		t.Fatalf("not applied: %+v", rep)
	}
	audio, _ := store.ListChapters(anchor)
	if got := titlesOf(audio); strings.Join(got, "|") != "Chapter I. Dawn|Chapter II. Noon|Chapter 3" {
		t.Errorf("audio rows: %v", got)
	}
	if audio[0].StartSec != 0 || audio[1].StartSec != 1000 || audio[1].EndSec != 2000 || audio[2].StartSec != 2000 || audio[2].Index != 2 {
		t.Errorf("audio row times/indices wrong: %+v", audio)
	}
	tr, _ := store.ListChaptersWithContent(trans)
	if got := titlesOf(tr); strings.Join(got, "|") != "Chapter I. Dawn|Chapter II. Noon|Chapter 3" {
		t.Errorf("transcript rows: %v", got)
	}
	// Sliced by time: row 0 holds words 0–999, row 1 words 1000–1999.
	if tr[0].WordCount != 1000 || tr[1].WordCount != 1000 ||
		!strings.HasPrefix(tr[0].Content, "w0") || !strings.Contains(tr[0].Content, "w999") || strings.Contains(tr[0].Content, "w1000") ||
		!strings.HasPrefix(tr[1].Content, "w1000") || !strings.Contains(tr[1].Content, "w1999") || strings.Contains(tr[1].Content, "w2000") {
		t.Errorf("transcript content not sliced from the timeline: wc %d/%d, %q / %q", tr[0].WordCount, tr[1].WordCount, tr[0].Content[:12], tr[1].Content[:12])
	}
	if tr[2].Content != "tail" {
		t.Errorf("the kept row lost its content: %q", tr[2].Content)
	}
	// Idempotent: the lead now begins at 0.
	again, _ := FillLeadingRows(store, work, ebook, true)
	if len(again.Added) != 0 || again.Skipped != "rows already begin at the start" {
		t.Errorf("second pass should be a no-op: %+v", again)
	}
}
