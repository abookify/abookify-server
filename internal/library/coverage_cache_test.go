package library

import (
	"encoding/json"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// GET /api/works calls BuildCoverage for every aligned work on every request,
// and each call used to parse the work's full anchor payloads (~190 MB across
// PJ's library → 6–22 s per list, past mobile's 20 s timeout). The cache must
// parse a row ONCE, give the same answer from the summary, and notice when the
// row is rewritten.
func TestAlignmentSummaryCacheParsesOnceAndTracksRewrites(t *testing.T) {
	store := testStoreForLib(t)
	wid, _ := store.CreateWork("Cached", "")
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/c.epub", Filename: "c.epub",
		Format: "epub", MediaType: "text", Origin: "publisher_epub"})
	store.UpsertBook(db.Book{WorkID: wid, Path: "/lib/c.txt", Filename: "c.txt",
		Format: "transcript", MediaType: "text", Origin: "whisper_transcript"})
	w, _ := store.GetWork(wid)
	ebook, trans := w.TextFiles[0].ID, w.TextFiles[1].ID
	payload := func(ebookOnly int) string {
		b, _ := json.Marshal(AnchorAlignmentPayload{Method: "anchor", Unit: "word",
			EbookWords: 100, TransWords: 100,
			Divergence: DivergenceSummary{EbookOnlyWords: ebookOnly, TransOnlyWords: 10}})
		return string(b)
	}
	save := func(ebookOnly int) {
		if err := store.SaveAlignment(db.Alignment{WorkID: wid, FromBookID: ebook, ToBookID: trans,
			Unit: "word", Method: "anchor", Confidence: 0.9, Pairs: payload(ebookOnly)}); err != nil {
			t.Fatal(err)
		}
	}
	save(20)

	cache := NewAlignmentSummaryCache()
	plain, err := BuildCoverage(store, wid)
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildCoverageWith(store, cache, wid)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BuildCoverageWith(store, cache, wid)
	if cache.Loads() != 1 {
		t.Fatalf("two cached builds must parse the payload once, parsed %d times", cache.Loads())
	}
	if len(first.Pairs) != 1 || len(plain.Pairs) != 1 {
		t.Fatalf("want 1 pair, cached %d uncached %d", len(first.Pairs), len(plain.Pairs))
	}
	if first.Pairs[0].DirectionalCoverage != plain.Pairs[0].DirectionalCoverage ||
		second.Pairs[0].DirectionalCoverage != plain.Pairs[0].DirectionalCoverage {
		t.Errorf("cached coverage differs from uncached:\n cached %+v\n plain  %+v",
			first.Pairs[0].DirectionalCoverage, plain.Pairs[0].DirectionalCoverage)
	}
	if first.Pairs[0].EbookToAudio != 0.8 {
		t.Errorf("ebook_to_audio: want 0.8, got %v", first.Pairs[0].EbookToAudio)
	}

	// A re-alignment rewrites the row (updated_at bumps, the blob changes):
	// the next build must parse again and report the new tally, never the old.
	save(50)
	third, _ := BuildCoverageWith(store, cache, wid)
	if cache.Loads() != 2 {
		t.Fatalf("a rewritten row must be re-parsed exactly once more, loads=%d", cache.Loads())
	}
	if third.Pairs[0].EbookToAudio != 0.5 {
		t.Errorf("after rewrite ebook_to_audio: want 0.5, got %v", third.Pairs[0].EbookToAudio)
	}

	// Warm on a fresh cache parses every row once; a second warm parses nothing.
	fresh := NewAlignmentSummaryCache()
	if n := fresh.Warm(store); n != 1 {
		t.Errorf("warm: want 1 row parsed, got %d", n)
	}
	if n := fresh.Warm(store); n != 0 {
		t.Errorf("second warm: want 0 rows parsed, got %d", n)
	}
	if _, err := BuildCoverageWith(store, fresh, wid); err != nil || fresh.Loads() != 1 {
		t.Errorf("build after warm must not parse again: loads=%d err=%v", fresh.Loads(), err)
	}
}
