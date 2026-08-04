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
	"time"

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

	// Rewriting the text invalidates the alignment that describes it. If this
	// process dies between the two, or the realign errors, the work is left with new
	// text and an alignment payload pointing into text that no longer exists — a
	// state nothing surfaces, and which presents to a reader as karaoke silently not
	// working. So the exit status has to mean "verified", not "reached the end":
	// anything that leaves the alignment older than this run fails loudly, and the
	// caller must not record the book as done.
	started := time.Now().Add(-2 * time.Second)

	sidecar, err := library.ReimportWorkSidecar(store, *libRoot, *workID)
	if err != nil {
		log.Fatalf("reimport work %d: %v", *workID, err)
	}
	fmt.Printf("reimported sidecar: %s\n", sidecar)

	rag, ragErr := buildRAG(store)
	if ragErr != nil {
		log.Printf("WARNING: no RAG client (%v) — embeddings depend on the server's backfill", ragErr)
	}
	if err := embedWork(store, rag, *workID); err != nil {
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

	// A cross-translation work carries an embedding/paragraph row alongside the
	// anchor row (Republic, Meditations, Iliad, Hero with a Thousand Faces). That
	// row points at the transcript too, so the reimport staled it just the same —
	// left alone it fails the freshness check below, and rightly: paragraph-follow
	// karaoke would run on the dead text. Refresh it when present.
	if rows, err := store.ListAlignmentsForWork(*workID); err == nil {
		for _, a := range rows {
			if a.Method != "embedding" {
				continue
			}
			var embedder library.ChunkEmbedder
			if rag != nil {
				embedder = rag
			}
			ecov, quality, err := library.ComputeEmbeddingAlignment(store, embedder, *workID)
			if err != nil {
				log.Fatalf("embedding realign work %d: %v", *workID, err)
			}
			fmt.Printf("embedding alignment refreshed: coverage %.4f quality %.2f\n", ecov, quality)
			break
		}
	}

	// Local-first sync contract: content_version must move on ANY data change,
	// or mobile's update-check never re-fetches the work. The post-STT hook in
	// the server stamps; this out-of-band path has to stamp for itself, or a
	// re-transcribed + re-aligned work stays invisible to already-synced
	// devices.
	if err := store.StampVersions(*workID, abook.BookDBSchemaVersion); err != nil {
		log.Fatalf("stamp versions for work %d: %v", *workID, err)
	}
	if err := verifyAlignmentFresh(store, *workID, started); err != nil {
		log.Fatalf("ALIGNMENT NOT VERIFIED for work %d: %v\n"+
			"The text was rewritten. Do NOT record this book as done — its alignment "+
			"describes text that no longer exists, which reads as karaoke being broken.",
			*workID, err)
	}
	fmt.Printf("stamped content_version (schema v%d)\n", abook.BookDBSchemaVersion)
}

// verifyAlignmentFresh confirms every alignment row for the work was rewritten by
// this run. A work with no ebook peer legitimately has no rows — that is verified
// too, and reported, rather than being indistinguishable from a failure.
func verifyAlignmentFresh(store *db.Store, workID int64, started time.Time) error {
	rows, err := store.ListAlignmentsForWork(workID)
	if err != nil {
		return fmt.Errorf("read alignments: %w", err)
	}
	if len(rows) == 0 {
		w, err := store.GetWork(workID)
		if err != nil || w == nil {
			return fmt.Errorf("get work: %w", err)
		}
		for _, b := range w.TextFiles {
			if b.Format != "transcript" && b.Origin != "whisper_transcript" {
				return fmt.Errorf("work has an ebook peer (%s) but no alignment row was "+
					"written — the aligner did not run or produced nothing", b.Filename)
			}
		}
		fmt.Printf("alignment: none expected (no ebook peer to align against) — verified\n")
		return nil
	}
	for _, a := range rows {
		if a.UpdatedAt.Before(started) {
			return fmt.Errorf("alignment %d (books %d→%d, %s/%s) last updated %s, before this "+
				"run began at %s — it describes the previous transcript",
				a.ID, a.FromBookID, a.ToBookID, a.Method, a.Unit,
				a.UpdatedAt.Format(time.RFC3339), started.Format(time.RFC3339))
		}
	}
	fmt.Printf("alignment: %d row(s) rewritten by this run — verified\n", len(rows))
	return nil
}

// buildRAG builds the RAG client from settings exactly as the server's
// ReloadLLM does (vault credential first, then the legacy inline key, then env).
func buildRAG(store *db.Store) (*llm.RAG, error) {
	settings, err := store.GetAllSettings()
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
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
		return nil, fmt.Errorf("no LLM provider configured")
	}
	return llm.NewRAG(store, llm.NewClient(llm.Provider(provider),
		apiKey, settings["llm_model"], settings["llm_base_url"])), nil
}

// embedWork fills embeddings for every text book of the work. EmbedBook only
// touches chunks whose embedding is empty, so this is cheap when nothing changed.
func embedWork(store *db.Store, rag *llm.RAG, workID int64) error {
	if rag == nil {
		return fmt.Errorf("no RAG client")
	}
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
