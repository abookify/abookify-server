// text-trust-sweep answers "does the transcript match the audio?" for every work
// and persists the verdict, so the library listing can show it without parsing
// sidecars per request.
//
//	docker run … go run ./cmd/text-trust-sweep -db ./data/abookify.db -library ./testdata/library
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

	var checked, unchecked int
	byState := map[string]int{}
	fmt.Printf("%-6s %-42s %-13s %9s %8s\n", "work", "title", "state", "suspect", "share")
	for _, w := range works {
		t, err := library.ComputeTextTrust(store, *libRoot, w.ID)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		if t == nil {
			unchecked++
			continue
		}
		checked++
		byState[t.State]++
		if t.State != library.TrustVerified {
			fmt.Printf("%-6d %-42s %-13s %9d %7.2f%%\n",
				w.ID, trunc(w.Title, 42), t.State, t.SuspectWords, t.SuspectPercent)
		}
	}
	fmt.Printf("\nchecked %d work(s); no sidecar (not transcribed): %d\n", checked, unchecked)
	for _, k := range []string{library.TrustVerified, library.TrustMinor,
		library.TrustSignificant, library.TrustUnchecked} {
		fmt.Printf("  %-13s %d\n", k, byState[k])
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
