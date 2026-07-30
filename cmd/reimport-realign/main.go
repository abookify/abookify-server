// reimport-realign lands ONE work's re-transcribed sidecar in the database:
// transcript + paragraphs, RAG chunks, embeddings, then anchor alignment. Reads
// the sidecar via the same /library→libraryRoot mapping the server uses
// (findSidecar). No STT/Whisper here.
//
//	docker run … go run ./cmd/reimport-realign -db ./data/abookify.db \
//	  -library ./testdata/library -work 32
//
// EMBEDDINGS ARE PART OF LANDING A BOOK, not a follow-up. Re-chunking writes rows
// with empty embeddings, and Q&A's vector search simply cannot see an unembedded
// chunk — so a repair that stopped at re-chunking would swap "Q&A quotes the wrong
// words" for "Q&A cannot find the book at all", which is not an improvement. During
// a long repair run there is no restart to trigger the server's own backfill, so
// this does it inline and the reader and Q&A are never out of step for a book that
// reports done.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
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

	if err := embedWork(store, *workID); err != nil {
		// Not fatal: the text is already correct and the server's own backfill will
		// fill these in. Loud, because until then Q&A is retrieval-blind on this book.
		log.Printf("WARNING: embeddings not filled for work %d: %v — Q&A vector search "+
			"cannot see this book until the server's backfill runs", *workID, err)
	}

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

// embedWork fills embeddings for every text book of the work, building the RAG
// client from settings exactly as the server's ReloadLLM does (vault credential
// first, then the legacy inline key, then env). EmbedBook only touches chunks whose
// embedding is empty, so this is cheap when nothing changed.
func embedWork(store *db.Store, workID int64) error {
	settings, err := store.GetAllSettings()
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	provider, apiKey := settings["llm_provider"], settings["llm_api_key"]
	if provider != "" && provider != "ollama" {
		if k := store.CredentialAPIKey(provider); k != "" {
			apiKey = k
		}
	}
	if provider == "" {
		if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
			provider, apiKey = "anthropic", k
		} else if k := os.Getenv("OPENAI_API_KEY"); k != "" {
			provider, apiKey = "openai", k
		}
	}
	if provider == "" || (provider != "ollama" && apiKey == "") {
		return fmt.Errorf("no LLM provider configured")
	}

	rag := llm.NewRAG(store, llm.NewClient(llm.Provider(provider),
		apiKey, settings["llm_model"], settings["llm_base_url"]))

	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		return fmt.Errorf("get work: %w", err)
	}
	for _, tf := range w.TextFiles {
		n, err := rag.EmbedBook(tf.ID)
		if err != nil {
			return fmt.Errorf("embed book %d (%s): %w", tf.ID, tf.Filename, err)
		}
		if n > 0 {
			fmt.Printf("embedded %d new chunk(s) in book %d (%s)\n", n, tf.ID, tf.Filename)
		}
	}
	return nil
}
