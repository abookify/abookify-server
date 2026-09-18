// timing-audit — the chapter-timing check across the library.
//
// For every work with a detected audio chapter list AND an aligned publisher
// ebook, compare each ebook chapter's alignment-timed start with the spoken
// announcement of the same chapter number. Coverage cannot see a timing fault
// (fe2833d: identical coverage while chapters ran minutes late); this can.
//
//	go run ./cmd/timing-audit [-db ./data/abookify.db] [-work N] [-all]
//
// Prints one row per work; exit 1 when any work with a verdict is not OK.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func short(t string) string {
	if len(t) > 30 {
		return t[:30]
	}
	return t
}

// outlier names the worst chapter when it is over tolerance on an otherwise
// healthy work — an ebook unit the narration never announces.
func outlier(r library.TimingReport) string {
	if r.WorstAbsSec > 30 {
		return fmt.Sprintf("outlier %q %.0fs", short(r.WorstTitle), r.WorstAbsSec)
	}
	return ""
}

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	only := flag.Int64("work", 0, "only this work id (0 = every work)")
	all := flag.Bool("all", false, "also print skipped works")
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
	fmt.Printf("%-5s %-40s %-7s %7s %8s %8s  %s\n", "work", "title", "verdict", "within", "median", "worst", "note")
	var ok, bad, skipped int
	// Silences are a signal: a class of work the audit declines to judge is
	// invisible to it, which reads exactly like passing unless someone counts.
	// (The audit skipped every dual-edition work for a day — 11 showcase
	// books — while reporting "24 ok".) Group no-verdict works by the SHAPE
	// they share: the skip reason with its numbers stripped.
	shape := map[string][]int64{}
	numbers := regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)
	for _, w := range works {
		if *only != 0 && w.ID != *only {
			continue
		}
		r, err := library.AuditChapterTiming(store, w.ID)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		title := w.Title
		if len(title) > 40 {
			title = title[:40]
		}
		switch {
		case r.Skipped != "":
			skipped++
			k := numbers.ReplaceAllString(r.Skipped, "N")
			shape[k] = append(shape[k], w.ID)
			if *all || *only != 0 {
				fmt.Printf("%-5d %-40s %-7s %7s %8s %8s  %s\n", w.ID, title, "skip", "-", "-", "-", r.Skipped)
			}
		case r.OK:
			ok++
			fmt.Printf("%-5d %-40s %-7s %3d/%-3d %7.1fs %7.1fs  %s\n", w.ID, title, "ok", r.Within, r.Compared, r.MedianAbsSec, r.WorstAbsSec, outlier(r))
		default:
			bad++
			fmt.Printf("%-5d %-40s %-7s %3d/%-3d %7.1fs %7.1fs  worst %q\n", w.ID, title, "DRIFT", r.Within, r.Compared, r.MedianAbsSec, r.WorstAbsSec, r.WorstTitle)
		}
		if *only != 0 {
			for _, d := range r.Deltas {
				fmt.Printf("      text %-3d %-32q nearest audio %-3d %9.1fs  ebook %9.1fs  delta %+8.1fs\n", d.TextIndex, short(d.TextTitle), d.AudioIndex, d.AudioStartSec, d.TextStartSec, d.DeltaSec)
			}
		}
	}
	fmt.Printf("\n%d ok, %d DRIFT, %d skipped (no verdict)\n", ok, bad, skipped)
	if skipped > 0 {
		keys := make([]string, 0, len(shape))
		for k := range shape {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return len(shape[keys[i]]) > len(shape[keys[j]]) })
		fmt.Printf("no verdict, by shape (a shape the audit cannot judge is a shape it cannot fail):\n")
		for _, k := range keys {
			ids := shape[k]
			show := ids
			if len(show) > 8 {
				show = show[:8]
			}
			fmt.Printf("  %3d  %-70s works %v%s\n", len(ids), k, show, map[bool]string{true: " …", false: ""}[len(ids) > 8])
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
}
