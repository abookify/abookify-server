// align-cli runs the anchor aligner (internal/library.ComputeAnchorAlignment)
// against works that have both an ebook and a transcript, and upserts the
// result into the alignments table. Use it to (re)build alignments after a
// transcription lands, or to backfill existing works.
//
//	align-cli                 align every work that has both peers
//	align-cli --work 27       align just work 27
//	align-cli --db path.db    point at a specific database
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
)

func main() {
	dbPath := flag.String("db", envOr("ABOOKIFY_DB_PATH", "./data/abookify.db"), "path to SQLite database")
	workID := flag.Int64("work", 0, "align only this work ID (0 = all eligible works)")
	embed := flag.Bool("embed", false, "when lexical anchor coverage is below --threshold, fall back to embedding+DTW (cross-translation) alignment")
	embedURL := flag.String("embed-url", "http://localhost:11434", "Ollama base URL for embeddings (nomic-embed-text)")
	threshold := flag.Float64("threshold", 0.25, "lexical coverage below which the embedding fallback runs (with --embed)")
	flag.Parse()

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	var embedder library.ChunkEmbedder
	if *embed {
		embedder = llm.NewRAG(store, llm.NewClient(llm.ProviderOllama, "", "", *embedURL))
	}

	var workIDs []int64
	if *workID != 0 {
		workIDs = []int64{*workID}
	} else {
		workIDs = eligibleWorks(store)
		if len(workIDs) == 0 {
			log.Fatalf("no works have both a publisher ebook and a whisper transcript")
		}
	}

	for _, id := range workIDs {
		title := ""
		if w, _ := store.GetWork(id); w != nil {
			title = w.Title
		}
		cov, err := library.ComputeAnchorAlignment(store, id)
		if err != nil {
			log.Printf("work %d: anchor ERROR %v", id, err)
			continue
		}
		fmt.Printf("work %-4d anchor %5.1f%%  %s\n", id, cov*100, title)

		// Coverage-driven routing: lexical alignment failed (different
		// translation/edition, or unrelated text) — try semantic alignment.
		if embedder != nil && cov < *threshold {
			ecov, mq, err := library.ComputeEmbeddingAlignment(store, embedder, id)
			if err != nil {
				log.Printf("work %d: embedding ERROR %v", id, err)
				continue
			}
			verdict := "different book (low similarity)"
			if mq >= embeddingSameWorkCutoff {
				verdict = "same work, different translation"
			}
			fmt.Printf("work %-4d   ↳ embedding %5.1f%% coverage, match-quality %.3f → %s\n", id, ecov*100, mq, verdict)
		}
	}
}

// embeddingSameWorkCutoff: mean matched-pair cosine above this means the two
// texts are the same work in a different translation (alignable at paragraph
// level); below it they're genuinely different texts.
const embeddingSameWorkCutoff = 0.7

// eligibleWorks returns work IDs that have both a publisher ebook/mobi and a
// whisper transcript — the pairs the anchor aligner can act on.
func eligibleWorks(store *db.Store) []int64 {
	books, err := store.ListBooks()
	if err != nil {
		log.Fatalf("list books: %v", err)
	}
	hasEbook := map[int64]bool{}
	hasTrans := map[int64]bool{}
	for _, b := range books {
		switch b.Origin {
		case "publisher_epub", "publisher_mobi":
			hasEbook[b.WorkID] = true
		case "whisper_transcript":
			hasTrans[b.WorkID] = true
		}
	}
	var ids []int64
	for wid := range hasEbook {
		if hasTrans[wid] {
			ids = append(ids, wid)
		}
	}
	return ids
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
