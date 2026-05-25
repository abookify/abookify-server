// align-cli runs the anchor aligner (internal/library.ComputeAnchorAlignment)
// against works that have both an ebook and a transcript, and upserts the
// result into the alignments table. Use it to (re)build alignments after a
// transcription lands, or to backfill existing works.
//
//	align-cli                 align every work that has both peers
//	align-cli --work 27       align just work 27
//	align-cli --db path.db    point at a specific database
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", envOr("ABOOKIFY_DB_PATH", "./data/abookify.db"), "path to SQLite database")
	workID := flag.Int64("work", 0, "align only this work ID (0 = all eligible works)")
	flag.Parse()

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	var workIDs []int64
	if *workID != 0 {
		workIDs = []int64{*workID}
	} else {
		workIDs = eligibleWorks(store)
		if len(workIDs) == 0 {
			log.Fatalf("no works have both a publisher ebook and a whisper transcript")
		}
	}

	for _, id := range workIDs {
		cov, err := library.ComputeAnchorAlignment(store, id)
		if err != nil {
			log.Printf("work %d: ERROR %v", id, err)
			continue
		}
		w, _ := store.GetWork(id)
		title := ""
		if w != nil {
			title = w.Title
		}
		fmt.Printf("work %-4d coverage %5.1f%%  %s\n", id, cov*100, title)
	}
}

// eligibleWorks returns work IDs that have both a publisher ebook/mobi and a
// whisper transcript — the pairs the anchor aligner can act on.
func eligibleWorks(store *db.Store) []int64 {
	books, err := store.ListBooks()
	if err != nil {
		log.Fatalf("list books: %v", err)
	}
	hasEbook := map[int64]bool{}
	hasTrans := map[int64]bool{}
	for _, b := range books {
		switch b.Origin {
		case "publisher_epub", "publisher_mobi":
			hasEbook[b.WorkID] = true
		case "whisper_transcript":
			hasTrans[b.WorkID] = true
		}
	}
	var ids []int64
	for wid := range hasEbook {
		if hasTrans[wid] {
			ids = append(ids, wid)
		}
	}
	return ids
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
