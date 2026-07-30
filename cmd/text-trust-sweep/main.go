// text-trust-sweep answers "does the transcript match the audio?" for every work
// and persists the verdict, so the library listing can show it without parsing
// sidecars per request.
//
//	docker run … go run ./cmd/text-trust-sweep -db ./data/abookify.db -library ./testdata/library
//
// -work limits the sweep to one work. Needed to re-measure a single book after
// repairing it: a full sweep would overwrite every other work's stored verdict,
// destroying the before-figures the repair is being judged against.
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
	only := flag.Int64("work", 0, "only this work id (0 = every work)")
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
		if *only != 0 && w.ID != *only {
			continue
		}
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
		// A full sweep lists only the books with something wrong. Asking about ONE
		// book has to print it either way — a repaired book comes back verified, and
		// silence is the one answer that cannot be told apart from a failed run.
		if *only != 0 || t.State != library.TrustVerified {
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
