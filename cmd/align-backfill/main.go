// align-backfill re-runs anchor alignment over every work, in place, to
// backfill payload fields added after the existing alignments were computed
// (e.g. the #209 render-ready `timeline`). It is CPU-only — it does NOT invoke
// STT/Whisper; it re-aligns the already-stored ebook ↔ transcript chapters.
//
// Safe to run against a live DB: db.Open uses WAL + busy_timeout(15s), so it
// coexists with a running server (which is idle w.r.t. alignment writes).
//
// Only the word/anchor rows are refreshed here. The embedding/paragraph rows
// (cross-translation works) need ComputeEmbeddingAlignment with an embedder —
// out of scope for this no-key tool; see handoff.
//
// Usage (via Docker, no host Go):
//
//	docker run --rm -v "$(pwd)":/app -w /app golang:1.24-bookworm \
//	  go run -buildvcs=false ./cmd/align-backfill -db ./data/abookify.db
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "path to the SQLite database")
	flag.Parse()

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer store.Close()

	works, err := store.ListWorks()
	if err != nil {
		log.Fatalf("list works: %v", err)
	}

	var aligned, skipped, errored int
	for _, w := range works {
		cov, err := library.ComputeAnchorAlignment(store, w.ID)
		if err != nil {
			errored++
			log.Printf("work %d %q: ERROR %v", w.ID, w.Title, err)
			continue
		}
		if cov == 0 {
			skipped++ // no ebook/transcript pair
			continue
		}
		aligned++
		log.Printf("work %d %q: anchor coverage %.3f (timeline backfilled)", w.ID, w.Title, cov)
	}
	fmt.Printf("\nalign-backfill done: aligned=%d skipped=%d errored=%d (of %d works)\n",
		aligned, skipped, errored, len(works))
}
