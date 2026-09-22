// store-reconcile is the READ-ONLY sweep that says whether the two stores —
// the SQLite rows and the files they point at — still agree, in BOTH
// directions (board 18):
//
//	rows without files   a book row whose file is gone (the phantom-position class)
//	rows without rows    a reference whose target row is gone (positions, bookmarks,
//	                     conditions, provenance, … — the tables the boot sweep skips)
//	files without rows   media no row indexes; edition dirs no row serves
//	derived              caches/sidecars/CAS objects that outlived their owner
//
// It is a reporter, not a cleaner: it opens the database read-only and never
// deletes, renames or writes. Deleting on inference is the fault class the
// August purge reconciliation was about; a person reads this and decides.
//
// Run it where the server's paths resolve (inside the server container, or on
// the desktop bundle's host):
//
//	docker exec server-server-1 go run ./cmd/store-reconcile -db /app/data/abookify.db -generated /generated
//	abookify-store-reconcile -db ~/.abookify/abookify.db -generated ~/.abookify/generated
//
// Exit status is 0 unless -fail-on names a severity that occurred (for a
// once-per-phase gate). Third member of the instrument family alongside
// testing/selfdesc-audit.md and testing/config-blindspots.md.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/pj/abookify/internal/db"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite database (opened read-only)")
	generated := flag.String("generated", "", "generated-audio dir as the server sees it (TTS editions, cas/, waveforms/); empty skips those checks")
	roots := flag.String("library", "", "comma-separated library roots to check INSTEAD of the library_roots table (host mapping)")
	examples := flag.Int("examples", 10, "sample paths/ids per finding")
	asJSON := flag.Bool("json", false, "emit the report as JSON")
	failOn := flag.String("fail-on", "", "exit 1 if a finding of this severity or worse exists: high | med")
	flag.Parse()

	// file: URI form — mode=ro is only honoured on a URI. busy_timeout so a
	// live server's writes don't turn the sweep into SQLITE_BUSY.
	sq, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=busy_timeout(15000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}
	defer sq.Close()
	if err := sq.Ping(); err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}

	opts := db.ReconcileOptions{GeneratedDir: *generated, MaxExamples: *examples}
	if *roots != "" {
		for _, r := range strings.Split(*roots, ",") {
			if r = strings.TrimSpace(r); r != "" {
				opts.LibraryRoots = append(opts.LibraryRoots, r)
			}
		}
	}
	rep, err := db.ReconcileStores(sq, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile:", err)
		os.Exit(2)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		fmt.Print(db.FormatReconcileReport(rep))
	}

	rank := map[string]int{"HIGH": 0, "MED": 1, "LOW": 2, "INFO": 3}
	if lim, ok := rank[strings.ToUpper(*failOn)]; ok {
		for _, f := range rep.Findings {
			if rank[f.Severity] <= lim {
				os.Exit(1)
			}
		}
	}
}
