package library

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// The instrument end to end, on the exact shape that drifted every
// human-narrated work (fe2833d): a narration whose Whisper words include glued
// tokens (no leading space), a publisher ebook of the same text, the real
// pipeline (ComputeAnchorAlignment), then the audit.
//
//   - healthy bake → OK, every ebook chapter on its announcement;
//   - the SAME alignment re-baked through the content-Fields map (the
//     pre-fe2833d code path) → DRIFT.
//
// Coverage is identical in both cases. That is the whole point of the audit:
// the metric shown to users cannot see this class; this test proves the
// audit can, so a regression of the bake goes red in `go test`, not in a
// listener's ear months later.
// buildGluedNarrationFixture creates a work with a narration whose Whisper
// words include glued tokens, a transcript, a publisher ebook of the same
// text (8 chapters of growing length) and detected chapters at the
// announcements. Returns the work + book ids.
const chapters = 8 // fixture chapters

var lastFixtureTimeline []db.SyncTimestamp

func buildGluedNarrationFixture(t *testing.T, store *db.Store) (workID, audioID, transID, ebookID int64) {
	t.Helper()

	// Chapter k has 150+45k words: lengths that vary the way real chapters do,
	// so a drift can never alias onto a neighbouring announcement for every
	// chapter at once (uniform lengths let a one-chapter drift score 0 s).
	const wordSec = 0.4
	wordsIn := func(k int) int { return 150 + 45*k }
	totalWords := 0
	for k := 0; k < chapters; k++ {
		totalWords += wordsIn(k)
	}

	var err error
	workID, err = store.CreateWork("Timing fixture", "Nobody")
	if err != nil {
		t.Fatal(err)
	}
	total := float64(totalWords) * wordSec
	for _, b := range []db.Book{
		{WorkID: workID, Path: "/lib/timing/narration.mp3", Filename: "narration.mp3", Format: "mp3", MediaType: "audio", Origin: "narrator_recording", Duration: total},
		{WorkID: workID, Path: "/lib/timing/narration.transcript", Filename: "narration.transcript", Format: "transcript", MediaType: "text", Origin: "whisper_transcript"},
		{WorkID: workID, Path: "/lib/timing/book.epub", Filename: "book.epub", Format: "epub", MediaType: "text", Origin: "publisher_epub"},
	} {
		if err := store.UpsertBook(b); err != nil {
			t.Fatal(err)
		}
	}
	books, _ := store.ListBooks()
	for _, b := range books {
		switch b.Path {
		case "/lib/timing/narration.mp3":
			audioID = b.ID
		case "/lib/timing/narration.transcript":
			transID = b.ID
		case "/lib/timing/book.epub":
			ebookID = b.ID
		}
	}
	if audioID == 0 || transID == 0 || ebookID == 0 {
		t.Fatal("book rows missing")
	}

	// Deterministic prose: a seeded generator over a 500-word vocabulary so
	// 4-grams anchor. Every third body word arrives glued ("-word", no leading
	// space) — joinWords fuses it onto its predecessor in the transcript
	// content, so the content has one whitespace word where the timeline has
	// two. A third of every chapter's words glue, so on the old map the drift
	// grows through the book (chapter 8 starts ~4.5 min early) — the LotR shape.
	seed := uint32(12345)
	next := func() uint32 {
		seed = seed*1664525 + 1013904223
		return seed
	}
	vocab := make([]string, 500)
	for i := range vocab {
		var sb strings.Builder
		n := 4 + int(next()%4)
		for j := 0; j < n; j++ {
			sb.WriteByte(byte('a' + next()%26))
		}
		vocab[i] = sb.String()
	}

	var tl []db.SyncTimestamp
	lastFixtureTimeline = nil
	push := func(w string) {
		s := float64(len(tl)) * wordSec
		tl = append(tl, db.SyncTimestamp{Word: w, Start: s, End: s + 0.3})
	}
	for k := 0; k < chapters; k++ {
		start := len(tl)
		push(" Chapter")
		push(fmt.Sprintf(" %d.", k+1))
		for i := 2; i < wordsIn(k); i++ {
			w := vocab[next()%uint32(len(vocab))]
			if i%3 == 2 {
				push("-" + w)
			} else {
				push(" " + w)
			}
		}
		text := joinWords(tl[start:])
		startSec, endSec := tl[start].Start, float64(len(tl))*wordSec
		title := fmt.Sprintf("Chapter %d", k+1)
		for _, ch := range []db.Chapter{
			{BookID: audioID, Index: k, Title: title, Src: "detected", StartSec: startSec, EndSec: endSec, Confidence: 0.9},
			{BookID: transID, Index: k, Title: title, Content: text, WordCount: wordsIn(k), StartSec: startSec, EndSec: endSec},
			{BookID: ebookID, Index: k, Title: title, Content: text, WordCount: wordsIn(k)},
		} {
			if err := store.InsertChapter(ch); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, _ := json.Marshal(tl)
	if err := store.SaveSyncData(workID, audioID, 0, string(raw)); err != nil {
		t.Fatal(err)
	}
	lastFixtureTimeline = tl

	return workID, audioID, transID, ebookID
}

func TestChapterTimingAudit_EndToEnd(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	workID, audioID, transID, ebookID := buildGluedNarrationFixture(t, store)
	_ = audioID

	cov, err := ComputeAnchorAlignment(store, workID)
	if err != nil {
		t.Fatalf("align: %v", err)
	}
	if cov < 0.95 {
		t.Fatalf("coverage %.3f — the fixture should align nearly whole", cov)
	}

	healthy, err := AuditChapterTiming(store, workID)
	if err != nil {
		t.Fatal(err)
	}
	if healthy.Skipped != "" {
		t.Fatalf("no verdict: %s", healthy.Skipped)
	}
	if !healthy.OK || healthy.Compared != chapters || healthy.Within != chapters || healthy.WorstAbsSec > 1 {
		t.Fatalf("healthy bake must land every chapter on its announcement: %+v", healthy)
	}

	// The bug, re-applied to the same alignment: bake through the content's
	// whitespace words instead of the timeline's own. Coverage is untouched —
	// only the times move.
	aligns, _ := store.ListAlignmentsForWork(workID)
	var row *db.Alignment
	for i := range aligns {
		if aligns[i].Unit == "word" && aligns[i].FromBookID == ebookID {
			row = &aligns[i]
		}
	}
	if row == nil {
		t.Fatal("no word alignment row")
	}
	var payload AnchorAlignmentPayload
	if err := json.Unmarshal([]byte(row.Pairs), &payload); err != nil {
		t.Fatal(err)
	}
	transChs, _ := loadContentChapters(store, transID, false)
	tl := lastFixtureTimeline
	bakeSegmentTimes(payload.Segments, tl, buildTokToFields(transChs), true)
	buildRenderTimeline(&payload)
	drifted, _ := json.Marshal(payload)
	row.Pairs = string(drifted)
	if err := store.SaveAlignment(*row); err != nil {
		t.Fatal(err)
	}
	if payload.Coverage != cov {
		t.Fatalf("coverage changed on re-bake (%.4f → %.4f); the drift must be invisible to coverage for this test to mean anything", cov, payload.Coverage)
	}

	bad, err := AuditChapterTiming(store, workID)
	if err != nil {
		t.Fatal(err)
	}
	if bad.Skipped != "" {
		t.Fatalf("drifted bake yielded no verdict: %s", bad.Skipped)
	}
	if bad.OK {
		t.Fatalf("the audit passed a drifted bake — it cannot see the fe2833d class: %+v", bad)
	}
	if bad.WorstAbsSec < 60 {
		t.Fatalf("expected late chapters minutes off, worst %.0fs: %+v", bad.WorstAbsSec, bad)
	}
	t.Logf("healthy: %d/%d within, median %.1fs worst %.1fs; drifted: %d/%d within, median %.0fs worst %.0fs — coverage %.3f both",
		healthy.Within, healthy.Compared, healthy.MedianAbsSec, healthy.WorstAbsSec,
		bad.Within, bad.Compared, bad.MedianAbsSec, bad.WorstAbsSec, cov)
}

// Once the chain is trusted, the publisher edition's chapter NAMES land on the
// narration's canonical chapter rows by time; a publisher title that only
// numbers the chapter leaves the spoken title alone.
func TestPropagateEbookTitles_ByTime(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	workID, audioID, transID, ebookID := buildGluedNarrationFixture(t, store)
	if _, err := ComputeAnchorAlignment(store, workID); err != nil {
		t.Fatal(err)
	}
	work, _ := store.GetWork(workID)
	// idx 3 + 5 say no more than "Chapter N" (digits, spelled out) and must not
	// propagate; idx 6 is the book's OWN unit name (Carol's staves) and must.
	names := map[int]string{0: "The Eve of the War", 1: "The Falling Star", 3: "Chapter 4", 5: "Chapter Six", 6: "STAVE\u00a0SEVEN.", 7: "The Heat-Ray"}
	for idx, n := range names {
		if err := store.UpdateChapterTitle(ebookID, idx, n); err != nil {
			t.Fatal(err)
		}
	}
	changes, err := PropagateEbookTitles(store, work, ebookID, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0: "The Eve of the War", 1: "The Falling Star", 2: "Chapter 3", 3: "Chapter 4", 5: "Chapter 6", 6: "STAVE SEVEN.", 7: "The Heat-Ray"}
	for _, bookID := range []int64{audioID, transID} {
		chs, _ := store.ListChapters(bookID)
		got := map[int]string{}
		for _, c := range chs {
			got[c.Index] = c.Title
		}
		for idx, w := range want {
			if got[idx] != w {
				t.Errorf("book %d ch %d = %q, want %q (changes: %v)", bookID, idx, got[idx], w, changes)
			}
		}
	}
	if len(changes) != 8 { // 4 names × 2 books (audio anchor + transcript)
		t.Errorf("changes = %d, want 8: %v", len(changes), changes)
	}
}
