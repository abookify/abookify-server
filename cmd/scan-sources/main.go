// scan-sources decodes every audio file in the library and records whether it is
// readable, populating the source_scans table that drives the gap indicator's
// cause classification.
//
// Without this data every work reports cause "unknown", which the UI correctly
// renders as "we have not checked" — honest, but it means a shipped feature sits
// inert. Runs at roughly 760x realtime; PJ's 620-hour library takes minutes.
//
//	docker run … go run ./cmd/scan-sources -db ./data/abookify.db -library ./testdata/library [-work N]
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
	workID := flag.Int64("work", 0, "scan only this work (0 = all)")
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

	var totalFiles, totalDamaged, scannedWorks int
	for _, w := range works {
		if *workID != 0 && w.ID != *workID {
			continue
		}
		n, dmg, err := library.ScanWorkSources(store, *libRoot, w.ID)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		if n == 0 {
			continue
		}
		scannedWorks++
		totalFiles += n
		totalDamaged += dmg
		if dmg > 0 {
			fmt.Printf("  DAMAGED  work %d %-44s %d/%d file(s)\n", w.ID, trunc(w.Title, 44), dmg, n)
		}
	}
	fmt.Printf("\n%d work(s), %d audio file(s) scanned, %d damaged\n",
		scannedWorks, totalFiles, totalDamaged)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
