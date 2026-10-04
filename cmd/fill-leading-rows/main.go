// fill-leading-rows — complete a narration's leading chapter rows from its
// ebook alignment (library.FillLeadingRows), for one work or every work with
// a word alignment. The same step runs at the end of every anchor alignment;
// this applies it to works aligned before it existed. Dry by default.
//
//	go run ./cmd/fill-leading-rows [-db ./data/abookify.db] [-work N] [-apply]
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
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	only := flag.Int64("work", 0, "only this work id (0 = every work with a word alignment)")
	apply := flag.Bool("apply", false, "write the rows (default: report only)")
	flag.Parse()
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	works, err := store.ListWorks()
	if err != nil {
		log.Fatal(err)
	}
	late, applied := 0, 0
	for _, w := range works {
		if *only != 0 && w.ID != *only {
			continue
		}
		work, err := store.GetWork(w.ID)
		if err != nil || work == nil {
			continue
		}
		var ebookID int64
		for _, b := range work.TextFiles {
			if b.Format == "epub" {
				ebookID = b.ID
				break
			}
		}
		if ebookID == 0 {
			continue
		}
		rep, err := library.FillLeadingRows(store, work, ebookID, !*apply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "work %d: %v\n", w.ID, err)
			continue
		}
		if rep.Skipped == "rows already begin at the start" || rep.Skipped == "no narration book carries chapter rows" {
			continue
		}
		late++
		if rep.Applied {
			applied++
		}
		fmt.Printf("work %d %q: first row at %.0f s, transcript %d, audio %d — %d row(s) to add%s\n", w.ID, w.Title, rep.FirstRowSec, rep.TranscriptID, rep.AudioBookID, len(rep.Added), map[bool]string{true: " (APPLIED)", false: ""}[rep.Applied])
		for _, a := range rep.Added {
			fmt.Printf("    + %s\n", a)
		}
		if rep.Skipped != "" {
			fmt.Printf("    skipped: %s\n", rep.Skipped)
		}
	}
	fmt.Printf("\n%d work(s) with late-starting rows; %d applied\n", late, applied)
}
