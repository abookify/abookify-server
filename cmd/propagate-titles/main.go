// propagate-titles — copy the publisher ebook's chapter names onto the
// narration's canonical chapter rows by time (library.PropagateEbookTitles),
// for one work or the whole library. The same step runs at the end of every
// anchor alignment; this applies it to works aligned before it existed.
//
//	go run ./cmd/propagate-titles [-db ./data/abookify.db] [-work N] [-dry]
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	only := flag.Int64("work", 0, "only this work id (0 = every work)")
	dry := flag.Bool("dry", false, "report changes without writing")
	flag.Parse()
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()
	works, err := store.ListWorks()
	if err != nil {
		log.Fatalf("list works: %v", err)
	}
	total := 0
	for _, w := range works {
		if *only != 0 && w.ID != *only {
			continue
		}
		full, err := store.GetWork(w.ID)
		if err != nil || full == nil {
			continue
		}
		var ebook *db.Book
		for i := range full.TextFiles {
			b := &full.TextFiles[i]
			switch b.Origin {
			case "publisher_epub", "publisher_mobi", "publisher_pdf":
				if ebook == nil || db.OriginAuthority(b.Origin) > db.OriginAuthority(ebook.Origin) {
					ebook = b
				}
			}
		}
		if ebook == nil {
			continue
		}
		changes, err := library.PropagateEbookTitles(store, full, ebook.ID, *dry)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		if len(changes) == 0 {
			continue
		}
		fmt.Printf("== work %d %s (%d change(s)%s)\n", w.ID, w.Title, len(changes), map[bool]string{true: ", DRY RUN", false: ""}[*dry])
		for _, c := range changes {
			fmt.Printf("   %s\n", c)
		}
		total += len(changes)
	}
	fmt.Printf("\n%d title(s) %s\n", total, map[bool]string{true: "would change", false: "changed"}[*dry])
}
