// rechunk re-derives a work's ebook chunks from the (now clean) chapter text,
// clearing stale chunks that still held HTML-entity / footnote artifacts. Pure
// (no LLM); embeddings are re-applied afterward via the server's /embed endpoint
// (new chunks come back with empty embeddings, which RAG.EmbedBook fills).
//
//	docker run … go run ./cmd/rechunk -db ./data/abookify.db -work 53
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/db"
	lib "github.com/pj/abookify/internal/library"
)

func main() {
	dbPath := flag.String("db", "./data/abookify.db", "SQLite db path")
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
	for i := range w.TextFiles {
		b := &w.TextFiles[i]
		if b.Format != "epub" {
			continue
		}
		if err := store.DeleteChunksByBook(b.ID); err != nil {
			log.Fatalf("delete chunks book %d: %v", b.ID, err)
		}
		if err := lib.ChunkBook(store, b.ID); err != nil { // re-chunks (count is 0 now)
			log.Fatalf("rechunk book %d: %v", b.ID, err)
		}
		n, _ := store.ChunkCount(b.ID)
		fmt.Printf("work %d book %d (%s): re-chunked → %d chunks (embeddings now empty, run /embed)\n",
			*workID, b.ID, b.Format, n)
	}
}
