// sidecar-drift answers, for every work, whether the database still says what the
// sidecar on disk says — the question that had no detector when Atlas Shrugged
// served a discarded decode to the reader and Q&A for hours after being repaired.
//
// Read-only. Prints the exact command to fix each affected work rather than acting,
// because reimporting re-splits chapters and that is a decision, not a cleanup.
//
//	docker run … go run ./cmd/sidecar-drift -db ./data/abookify.db -library ./testdata/library
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
	libRoot := flag.String("library", "./testdata/library", "host library root that /library maps to")
	all := flag.Bool("all", false, "list every work, not only the affected ones")
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

	byState := map[string]int{}
	var affected []*library.SidecarDrift
	for _, w := range works {
		d, err := library.DetectSidecarDrift(store, *libRoot, w.ID)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		if d == nil {
			continue
		}
		byState[d.State]++
		if d.State != library.DriftOK && d.State != library.DriftNoSidecar {
			affected = append(affected, d)
		}
		if *all {
			fmt.Printf("%-5d %-44s %-13s match %6.2f%%  sidecar %d / db %d\n",
				d.WorkID, trunc(d.Title, 44), d.State, d.MatchPercent,
				d.SidecarWords, d.DBWords)
		}
	}

	fmt.Printf("\n%d work(s) examined\n", len(works))
	for _, k := range []string{library.DriftOK, library.DriftChunksOnly, library.DriftStale,
		library.DriftNotImported, library.DriftUnreadable, library.DriftNoSidecar} {
		fmt.Printf("  %-13s %d\n", k, byState[k])
	}

	if len(affected) == 0 {
		fmt.Printf("\nNo work is serving a decode its sidecar has superseded.\n")
		return
	}
	fmt.Printf("\nAFFECTED — the database is not serving what the sidecar says:\n")
	for _, d := range affected {
		fmt.Printf("\n  work %d  %s  [%s]\n    %s\n", d.WorkID, d.Title, d.State, d.Detail)
		fmt.Printf("    sidecar %d words / database %d words; n-gram match %.2f%%; chunks stale: %v\n",
			d.SidecarWords, d.DBWords, d.MatchPercent, d.ChunksStale)
		fmt.Printf("    fix: ./bin/reimport-realign -db %s -library %s -work %d\n",
			*dbPath, *libRoot, d.WorkID)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
