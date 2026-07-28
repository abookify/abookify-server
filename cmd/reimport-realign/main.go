// reimport-realign refreshes ONE work's transcript from its (re-transcribed)
// sidecar, then re-runs anchor alignment — for validating a GPU re-transcription
// against an ebook. Reads the sidecar via the same /library→libraryRoot mapping
// the server uses (findSidecar). CPU-only; no STT/Whisper here.
//
//	docker run … go run ./cmd/reimport-realign -db ./data/abookify.db \
//	  -library ./testdata/library -work 32
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "path to the SQLite database")
	libRoot := flag.String("library", "./testdata/library", "host library root that /library maps to")
	workID := flag.Int64("work", 0, "work id to reimport + realign")
	flag.Parse()
	if *workID == 0 {
		log.Fatal("-work is required")
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	sidecar, err := library.ReimportWorkSidecar(store, *libRoot, *workID)
	if err != nil {
		log.Fatalf("reimport work %d: %v", *workID, err)
	}
	fmt.Printf("reimported sidecar: %s\n", sidecar)

	cov, err := library.ComputeAnchorAlignment(store, *workID)
	if err != nil {
		log.Fatalf("realign work %d: %v", *workID, err)
	}
	fmt.Printf("anchor coverage after re-align: %.4f\n", cov)

	// Local-first sync contract: content_version must move on ANY data change,
	// or mobile's update-check never re-fetches the work. The post-STT hook in
	// the server stamps; this out-of-band path has to stamp for itself, or a
	// re-transcribed + re-aligned work stays invisible to already-synced
	// devices.
	if err := store.StampVersions(*workID, abook.BookDBSchemaVersion); err != nil {
		log.Fatalf("stamp versions for work %d: %v", *workID, err)
	}
	fmt.Printf("stamped content_version (schema v%d)\n", abook.BookDBSchemaVersion)
}
