package main

import (
	"flag"
	"log"
	"os"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
	"github.com/pj/abookify/internal/scanner"
	"github.com/pj/abookify/internal/server"
	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

func main() {
	libraryPath := flag.String("library", envOrDefault("ABOOKIFY_LIBRARY_PATH", "./library"), "path to book library")
	dbPath := flag.String("db", envOrDefault("ABOOKIFY_DB_PATH", "./data/abookify.db"), "path to SQLite database")
	port := flag.String("port", envOrDefault("ABOOKIFY_PORT", "7654"), "HTTP server port")
	ttsURL := flag.String("tts-url", envOrDefault("ABOOKIFY_TTS_URL", ""), "TTS service URL")
	sttURL := flag.String("stt-url", envOrDefault("ABOOKIFY_STT_URL", ""), "STT service URL")
	generatedPath := flag.String("generated", envOrDefault("ABOOKIFY_GENERATED_PATH", "./generated"), "path for generated audio")
	flag.Parse()

	log.Printf("abookify server starting")
	log.Printf("  library:   %s", *libraryPath)
	log.Printf("  database:  %s", *dbPath)
	log.Printf("  generated: %s", *generatedPath)
	log.Printf("  port:      %s", *port)

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Run initial scan with metadata extraction
	results, err := scanner.Scan(*libraryPath)
	if err != nil {
		log.Fatalf("failed to scan library: %v", err)
	}
	log.Printf("scan found %d supported files", len(results))

	for _, r := range results {
		if err := store.UpsertBook(r); err != nil {
			log.Printf("warning: failed to store %s: %v", r.Path, err)
		}
	}

	// Match audiobooks with ebooks and create works
	if err := library.MatchAndCreateWorks(store); err != nil {
		log.Printf("warning: matching failed: %v", err)
	}

	// Extract chapters from EPUB files
	allBooks, _ := store.ListBooks()
	for _, b := range allBooks {
		if b.Format != "epub" {
			continue
		}
		count, _ := store.ChapterCount(b.ID)
		if count > 0 {
			continue
		}
		chapters, err := library.ExtractEPUBChapters(b.Path, b.ID)
		if err != nil {
			log.Printf("warning: chapter extraction failed for %s: %v", b.Filename, err)
			continue
		}
		for _, ch := range chapters {
			store.InsertChapter(ch)
		}
		log.Printf("extracted %d chapters from %s", len(chapters), b.Filename)
	}

	// Populate embedded chapter markers for audio files that have them
	// (M4B, some MP3s with ID3 CHAP frames). Runs before linking so the
	// linker sees authoritative chapters.
	for _, b := range allBooks {
		if b.MediaType != "audio" {
			continue
		}
		if _, err := library.PopulateEmbeddedChapters(store, b); err != nil {
			log.Printf("warning: embedded chapter probe failed for %s: %v", b.Filename, err)
		}
	}

	// Link audio chapters to text chapters
	works, _ := store.ListWorks()
	for i := range works {
		if works[i].HasAudio && works[i].HasText {
			library.LinkChapters(store, &works[i])
		}
	}

	// Chunk text for RAG
	allBooks2, _ := store.ListBooks()
	for _, b := range allBooks2 {
		if b.Format == "epub" {
			library.ChunkBook(store, b.ID)
		}
	}

	// Clean up orphaned DB entries (files that no longer exist on disk)
	if removed, err := store.CleanupOrphanedBooks(); err == nil && removed > 0 {
		log.Printf("cleaned up %d orphaned book entries", removed)
	}

	// Set up HTTP server
	srv := server.New(store, *port)
	srv.LibraryDir = *libraryPath

	// Set up TTS/STT clients and generator
	var ttsClient *tts.Client
	var sttClient *stt.Client

	if *ttsURL != "" {
		ttsClient = tts.NewClient(*ttsURL)
		if err := ttsClient.Health(); err != nil {
			log.Printf("warning: TTS service not ready: %v (will retry when needed)", err)
		} else {
			log.Printf("TTS service connected: %s", *ttsURL)
		}
	}

	if *sttURL != "" {
		sttClient = stt.NewClient(*sttURL)
		if err := sttClient.Health(); err != nil {
			log.Printf("warning: STT service not ready: %v (will retry when needed)", err)
		} else {
			log.Printf("STT service connected: %s", *sttURL)
		}
	}

	if ttsClient != nil || sttClient != nil {
		srv.Generator = library.NewGenerator(store, ttsClient, sttClient, *generatedPath, func(job library.JobStatus) {
			srv.Events.Broadcast(server.Event{Type: "job_update", Data: job})
		})
		log.Printf("generation engine ready (TTS: %v, STT: %v)", ttsClient != nil, sttClient != nil)
	}

	// Extract cover art
	coversDir := *libraryPath + "/covers"
	for i := range works {
		library.ExtractCoversForWork(store, &works[i], coversDir)
	}

	// Set up LLM for Q&A (reads API keys from settings)
	setupLLM(store, srv)

	// Start file watcher for live library updates
	watcher, err := library.NewWatcher(store, *libraryPath, func() {
		srv.Events.Broadcast(server.Event{Type: "library_updated"})
	})
	if err != nil {
		log.Printf("warning: file watcher failed to start: %v", err)
	} else {
		watcher.Start()
		defer watcher.Close()
	}

	log.Printf("listening on :%s", *port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func setupLLM(store *db.Store, srv *server.Server) {
	settings, err := store.GetAllSettings()
	if err != nil {
		return
	}

	provider := settings["llm_provider"]
	apiKey := settings["llm_api_key"]

	// Also check environment variables as fallback
	if provider == "" {
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			provider = "anthropic"
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		} else if os.Getenv("OPENAI_API_KEY") != "" {
			provider = "openai"
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}

	if provider == "" || (provider != "ollama" && apiKey == "") {
		log.Printf("LLM not configured (set API key in Settings or via environment)")
		return
	}

	model := settings["llm_model"]
	baseURL := settings["llm_base_url"]

	client := llm.NewClient(llm.Provider(provider), apiKey, model, baseURL)
	srv.RAG = llm.NewRAG(store, client)

	log.Printf("LLM Q&A ready (provider: %s, model: %s)", provider, client.Model())
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
