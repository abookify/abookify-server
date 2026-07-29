// redetect-gaps re-runs gap + hole detection across every transcribed work,
// rewriting each stored verdict WITHOUT re-importing transcripts.
//
// A detector only helps the books it has examined. When the hole detector
// landed, every already-imported book still carried a verdict from the bucket
// scan alone — so "no problems shown" meant "not re-checked", the same false
// clean the hole detector exists to remove. Running it over the sample that
// happened to surface is not coverage.
//
//	docker run … go run ./cmd/redetect-gaps -db ./data/abookify.db -library ./testdata/library
package main

import (
	"encoding/json"
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

	type row struct {
		id            int64
		title         string
		before, after int
		minutes       float64
		newlyFlagged  bool
		cause         string
	}
	var rows []row
	var examined int

	for _, w := range works {
		before, after, err := library.RedetectWorkGaps(store, *libRoot, w.ID)
		if err != nil {
			log.Printf("work %d (%s): %v", w.ID, w.Title, err)
			continue
		}
		if before == 0 && after == 0 {
			// Either never transcribed, or examined and clean. Distinguish by
			// whether a sidecar-backed audio book exists.
			if len(w.AudioFiles) > 0 {
				examined++
			}
			continue
		}
		examined++

		var mins float64
		if raw, e := store.GetTranscriptionGaps(w.AudioFiles[0].ID); e == nil && raw != "" && raw != "[]" {
			var gs []library.TranscriptionGap
			if json.Unmarshal([]byte(raw), &gs) == nil {
				for _, g := range gs {
					mins += g.DurationSec / 60
				}
			}
		}
		rows = append(rows, row{
			id: w.ID, title: w.Title, before: before, after: after,
			minutes: mins, newlyFlagged: before == 0 && after > 0,
			cause: causeFor(store, w),
		})
	}

	fmt.Printf("\n%-4s %-40s %7s %7s %9s  %s\n", "work", "title", "before", "after", "minutes", "note")
	newly := 0
	for _, r := range rows {
		note := ""
		if r.newlyFlagged {
			note = "*** WAS SHOWING CLEAN ***"
			newly++
		} else if r.after > r.before {
			note = "more spans than before"
		} else if r.after < r.before {
			note = "fewer (recovered)"
		}
		if r.cause != "" {
			note = r.cause + "  " + note
		}
		fmt.Printf("%-4d %-40s %7d %7d %9.1f  %s\n", r.id, trunc(r.title, 40), r.before, r.after, r.minutes, note)
	}
	fmt.Printf("\nexamined %d work(s) with audio; %d now report spans; %d were previously showing CLEAN\n",
		examined, len(rows), newly)
}

// causeFor mirrors the summary endpoint's classification so the sweep reports
// the same verdict the UI will.
func causeFor(store *db.Store, w db.Work) string {
	ids := make([]int64, 0, len(w.AudioFiles))
	for _, b := range w.AudioFiles {
		ids = append(ids, b.ID)
	}
	scans, err := store.GetSourceScans(ids)
	if err != nil {
		return ""
	}
	var trunc, dmg, clean int
	for _, id := range ids {
		sc, ok := scans[id]
		if !ok {
			continue
		}
		switch {
		case sc.Truncated:
			trunc++
		case sc.DecodeErrors > 0:
			dmg++
		default:
			clean++
		}
	}
	switch {
	case trunc > 0:
		return "truncated_source"
	case dmg > 0:
		return "damaged_source"
	case clean == len(ids) && len(ids) > 0:
		return "dropped_segment"
	}
	return "unknown"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
