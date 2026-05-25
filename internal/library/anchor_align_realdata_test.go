package library

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// localDBPath is the dev database, relative to internal/library. The
// real-data alignment check runs only when it's present, so CI (which has
// no library DB) skips cleanly.
const localDBPath = "../../data/abookify.db"

// concatByOrigin joins all chapter content for the books of a work that have
// the given origin, in (book order, chapter index) order.
func concatByOrigin(t *testing.T, store *db.Store, workID int64, origin string) string {
	t.Helper()
	books, err := store.ListBooks()
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	var sb strings.Builder
	for _, b := range books {
		if b.WorkID != workID || b.Origin != origin {
			continue
		}
		chs, err := store.ListChapters(b.ID)
		if err != nil {
			t.Fatalf("ListChapters(%d): %v", b.ID, err)
		}
		sort.Slice(chs, func(i, j int) bool { return chs[i].Index < chs[j].Index })
		for _, ch := range chs {
			full, err := store.GetChapterContent(b.ID, ch.Index)
			if err != nil || full == nil {
				continue
			}
			sb.WriteString(full.Content)
			sb.WriteString(" ")
		}
	}
	return sb.String()
}

// TestAlign_RealData runs the anchor aligner against whatever ebook+transcript
// pairs exist locally (KC = work 27, Frankenstein = work 28 in PJ's dev DB)
// and reports coverage + the largest divergences. It asserts only loose
// sanity bounds — the point is to confirm the synthetic-test behavior holds
// on real GPU transcripts, and to surface the divergence map for eyeballing.
func TestAlign_RealData(t *testing.T) {
	if _, err := os.Stat(localDBPath); err != nil {
		t.Skip("no local dev DB; skipping real-data alignment check")
	}
	store, err := db.Open(localDBPath)
	if err != nil {
		t.Skipf("cannot open local DB: %v", err)
	}

	cases := []struct {
		workID int64
		label  string
	}{
		{27, "Kitchen Confidential"},
		{28, "Frankenstein"},
	}
	for _, c := range cases {
		ebook := Tokenize(concatByOrigin(t, store, c.workID, "publisher_epub"))
		trans := Tokenize(concatByOrigin(t, store, c.workID, "whisper_transcript"))
		if len(ebook) < 1000 || len(trans) < 1000 {
			t.Logf("%s (work %d): missing peer text (ebook=%d, trans=%d) — skipping", c.label, c.workID, len(ebook), len(trans))
			continue
		}
		a := Align(ebook, trans, 4)
		cov := a.Coverage(len(ebook))
		t.Logf("%s (work %d): ebook %d words, transcript %d words, %d anchors, coverage %.1f%%",
			c.label, c.workID, len(ebook), len(trans), len(a.Anchors), cov*100)

		// Report the biggest divergence segments — these are the audiobook
		// intros/outros and ebook front/back-matter the aligner found.
		type div struct {
			kind             SegmentKind
			eWords, tWords   int
			eStart           int
		}
		var divs []div
		for _, s := range a.Segments {
			if s.Kind == SegAligned {
				continue
			}
			divs = append(divs, div{s.Kind, s.EbookEnd - s.EbookStart, s.TransEnd - s.TransStart, s.EbookStart})
		}
		sort.Slice(divs, func(i, j int) bool {
			return divs[i].eWords+divs[i].tWords > divs[j].eWords+divs[j].tWords
		})
		for i, d := range divs {
			if i >= 5 {
				break
			}
			t.Logf("    divergence #%d: %-11s ebook+%d / trans+%d words (at ebook word %d)",
				i+1, d.kind, d.eWords, d.tWords, d.eStart)
		}

		// Loose sanity: content-matched works should cover most of the ebook.
		if cov < 0.5 {
			t.Errorf("%s: coverage %.1f%% is suspiciously low for a content-matched pair", c.label, cov*100)
		}
	}
}
