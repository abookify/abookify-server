// reextract-align re-extracts ONE work's EPUB chapters with the current
// extractor (HTML-entity decode + footnote/superscript stripping) and re-runs
// anchor alignment — to confirm the meld's false diffs (literal 'nbsp',
// 'four1', glued footnote markers) clear. CPU-only.
//
//	docker run … go run ./cmd/reextract-align -db ./data/abookify.db \
//	  -library ./testdata/library -work 53
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/pj/abookify/internal/db"
	lib "github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
	libRoot := flag.String("library", "./testdata/library", "host library root that /library maps to")
	workID := flag.Int64("work", 0, "work id")
	flag.Parse()
	if *workID == 0 {
		log.Fatal("-work required")
	}

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	w, err := store.GetWork(*workID)
	if err != nil || w == nil {
		log.Fatalf("work %d: %v", *workID, err)
	}
	var ep *db.Book
	for i := range w.TextFiles {
		if w.TextFiles[i].Format == "epub" {
			ep = &w.TextFiles[i]
			break
		}
	}
	if ep == nil {
		log.Fatalf("work %d has no epub", *workID)
	}
	host := strings.Replace(ep.Path, "/library/", strings.TrimRight(*libRoot, "/")+"/", 1)

	chapters, err := lib.ExtractEPUBChapters(host, ep.ID)
	if err != nil {
		log.Fatalf("extract %s: %v", host, err)
	}
	if err := store.DeleteChaptersByBook(ep.ID); err != nil {
		log.Fatalf("delete chapters: %v", err)
	}
	nbsp := 0
	for _, ch := range chapters {
		if strings.Contains(strings.ToLower(ch.Content), "nbsp") {
			nbsp++
		}
		if err := store.InsertChapter(ch); err != nil {
			log.Fatalf("insert chapter: %v", err)
		}
	}
	fmt.Printf("re-extracted %d chapters; chapters still containing 'nbsp': %d\n", len(chapters), nbsp)

	cov, err := lib.ComputeAnchorAlignment(store, *workID)
	if err != nil {
		log.Fatalf("realign: %v", err)
	}
	fmt.Printf("anchor coverage after re-extract + re-align: %.4f\n", cov)
}
