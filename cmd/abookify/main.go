package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/scanner"
	"github.com/pj/abookify/internal/server"
	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// "dev" for `go run` / un-stamped builds. Surfaced in the startup log and
// GET /api/info so the desktop shell can show + update-check the bundle.
var version = "dev"

func main() {
	// data-dir is the install root (the desktop bundle passes ~/.abookify).
	// It supplies the DEFAULTS for the per-path flags below; an explicit
	// --library / --db / --generated / env var still overrides its slot, so
	// the Docker compose (which sets ABOOKIFY_*_PATH explicitly) is unchanged.
	dataDir := flag.String("data-dir", envOrDefault("ABOOKIFY_DATA_DIR", defaultDataDir()), "root dir for db/library/generated/models (~/.abookify on desktop)")
	// Parse data-dir first so it can seed the others' defaults. (flag doesn't
	// support staged parsing, so resolve it from env directly here; the flag
	// is still registered for --help + explicit override.)
	root := envOrDefault("ABOOKIFY_DATA_DIR", defaultDataDir())

	libraryPath := flag.String("library", envOrDefault("ABOOKIFY_LIBRARY_PATH", filepath.Join(root, "library")), "path to book library")
	dbPath := flag.String("db", envOrDefault("ABOOKIFY_DB_PATH", filepath.Join(root, "abookify.db")), "path to SQLite database")
	port := flag.String("port", envOrDefault("ABOOKIFY_PORT", "7654"), "HTTP server port")
	ttsURL := flag.String("tts-url", envOrDefault("ABOOKIFY_TTS_URL", ""), "TTS service URL")
	sttURL := flag.String("stt-url", envOrDefault("ABOOKIFY_STT_URL", ""), "STT service URL")
	booknlpURL := flag.String("booknlp-url", envOrDefault("ABOOKIFY_BOOKNLP_URL", ""), "BookNLP cast service URL (experimental)")
	generatedPath := flag.String("generated", envOrDefault("ABOOKIFY_GENERATED_PATH", filepath.Join(root, "generated")), "path for generated audio")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	// Honor an explicit --data-dir override for the models path (the per-path
	// flags already resolved their own overrides above).
	modelsDir := filepath.Join(*dataDir, "models")

	// First-run: create the data dirs so a fresh ~/.abookify just works. The
	// DB open and initial scan both assume their parent dirs exist.
	for _, d := range []string{filepath.Dir(*dbPath), *libraryPath, *generatedPath, modelsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Printf("warning: could not create %s: %v", d, err)
		}
	}

	// Register the shutdown signals BEFORE the (possibly slow first-run) boot
	// so a quit during the initial scan is caught + buffered, not the default
	// hard-terminate. It's handled the moment boot reaches the serve loop.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	log.Printf("abookify server starting (version %s)", version)
	log.Printf("  data-dir:  %s", *dataDir)
	log.Printf("  library:   %s", *libraryPath)
	log.Printf("  database:  %s", *dbPath)
	log.Printf("  generated: %s", *generatedPath)
	log.Printf("  models:    %s", modelsDir)
	log.Printf("  port:      %s", *port)

	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Structured logging (#214): persist a recent window + tee stdlib log
	// into it so existing log.Printf calls show up in the System Console.
	applog.Init(store)
	applog.Info("system", "abookify server starting")

	// MOBI/AZW3/AZW → sibling .epub via calibre's ebook-convert (image
	// dep). Idempotent — skips files that already have a sibling .epub.
	// Lets the scanner + EPUB chapter extractor handle ebook formats
	// without a separate parser path.
	library.ConvertMobiFilesInDir(*libraryPath)

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

	// Import .stt.json sidecars produced by stt-cli (if any exist next to
	// audio files). Idempotent — skips works that already have sync_data.
	library.ImportSidecars(store, *libraryPath)

	// Propagate series metadata from EPUBs up to their parent works.
	// Runs once per boot — idempotent (skips works that already have series set).
	worksList, _ := store.ListWorks()
	for _, w := range worksList {
		if w.Series != "" {
			continue // already set (manual edit or prior run)
		}
		for _, tf := range w.TextFiles {
			if tf.Format != "epub" {
				continue
			}
			meta, err := library.ExtractEPUBMetadata(tf.Path)
			if err != nil || meta.Series == "" {
				continue
			}
			if err := store.SetSeries(w.ID, meta.Series, meta.SeriesIndex); err != nil {
				log.Printf("set-series for %q failed: %v", w.Title, err)
				continue
			}
			log.Printf("series: %q → %q #%.1f", w.Title, meta.Series, meta.SeriesIndex)
			break
		}
	}

	// Extract chapters from EPUB files (or re-extract if content_html is missing)
	allBooks, _ := store.ListBooks()
	for _, b := range allBooks {
		if b.Format != "epub" {
			continue
		}
		count, _ := store.ChapterCount(b.ID)
		needsExtract := count == 0
		if !needsExtract {
			// Check if existing chapters are missing HTML (pre-#102 data).
			needsExtract = store.HasChaptersMissingHTML(b.ID)
		}
		if !needsExtract {
			continue
		}
		// Clear old chapters before re-extracting.
		if count > 0 {
			store.DeleteChaptersByBook(b.ID)
		}
		chapters, err := library.ExtractEPUBChapters(b.Path, b.ID)
		if err != nil {
			log.Printf("warning: chapter extraction failed for %s: %v", b.Filename, err)
			continue
		}
		for _, ch := range chapters {
			store.InsertChapter(ch)
		}
		log.Printf("extracted %d chapters from %s (with HTML)", len(chapters), b.Filename)
		if _, err := library.PopulateParagraphsForBook(store, b.ID); err != nil {
			log.Printf("warning: paragraph population failed for %s: %v", b.Filename, err)
		}
	}

	// Extract chapters from plain-text files (.txt)
	for _, b := range allBooks {
		if b.Format != "txt" {
			continue
		}
		count, _ := store.ChapterCount(b.ID)
		if count > 0 {
			continue
		}
		chapters, err := library.ExtractTXTChapters(b.Path, b.ID)
		if err != nil {
			log.Printf("warning: txt chapter extraction failed for %s: %v", b.Filename, err)
			continue
		}
		for _, ch := range chapters {
			store.InsertChapter(ch)
		}
		log.Printf("extracted %d chapters from %s (txt)", len(chapters), b.Filename)
		if _, err := library.PopulateParagraphsForBook(store, b.ID); err != nil {
			log.Printf("warning: paragraph population failed for %s: %v", b.Filename, err)
		}
	}

	// Backfill paragraphs for any text book missing them in the background
	// so HTTP startup isn't blocked on large libraries. Idempotent — next
	// boot will skip books that were already populated.
	go func() {
		for _, b := range allBooks {
			if b.MediaType != "text" {
				continue
			}
			pcount, _ := store.ParagraphCount(b.ID)
			if pcount > 0 {
				continue
			}
			chcount, _ := store.ChapterCount(b.ID)
			if chcount == 0 {
				continue
			}
			library.PopulateParagraphsForBook(store, b.ID)
		}
	}()

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

	// Chunk text for RAG. Includes transcript books (whisper output split
	// into chapters) so audiobook-only works are searchable too.
	allBooks2, _ := store.ListBooks()
	for _, b := range allBooks2 {
		if b.Format == "epub" || b.Format == "transcript" {
			library.ChunkBook(store, b.ID)
		}
	}

	// Clean up orphaned DB entries (files that no longer exist on disk)
	if removed, err := store.CleanupOrphanedBooks(); err == nil && removed > 0 {
		log.Printf("cleaned up %d orphaned book entries", removed)
	}
	// Sweep content rows whose owning book is gone (debris from book
	// deletions that predate the cascade fix in CleanupOrphanedBooks).
	if removed, err := store.CleanupOrphanedRows(); err == nil && removed > 0 {
		log.Printf("cleaned up %d orphaned content rows (chunks/paragraphs/chapters)", removed)
	}

	// Set up HTTP server
	srv := server.New(store, *port)
	srv.Version = version
	srv.LibraryDir = *libraryPath
	srv.GeneratedDir = *generatedPath
	srv.BookNLPURL = *booknlpURL
	srv.DataDir = *dataDir
	srv.ModelsDir = modelsDir
	srv.TTSURL = *ttsURL
	srv.STTURL = *sttURL

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
		srv.Generator = library.NewGenerator(store, ttsClient, sttClient, *generatedPath, srv.OnJobUpdate)
		srv.Generator.SetLibraryRoot(*libraryPath)
		log.Printf("generation engine ready (TTS: %v, STT: %v)", ttsClient != nil, sttClient != nil)
	}

	// Extract cover art
	coversDir := *libraryPath + "/covers"
	for i := range works {
		library.ExtractCoversForWork(store, &works[i], coversDir)
	}

	// Set up LLM for Q&A (reads API keys from settings + env fallbacks).
	// handleSaveSettings calls the same ReloadLLM so adding a key in the UI
	// takes effect without a restart.
	srv.ReloadLLM()

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

	// Start ingest queue: file-based drop-zone at <library>/incoming/.
	// Users put audiobooks/ebooks there; the queue copies them into the
	// canonical audiobooks/ or ebooks/ subdirectories, where the library
	// watcher above picks them up for normal scan/import.
	ingest, err := library.NewIngestQueue(*libraryPath)
	if err != nil {
		log.Printf("warning: ingest queue failed to start: %v", err)
	} else {
		ingest.SetOnChange(func() {
			srv.Events.Broadcast(server.Event{Type: "queue_updated"})
		})
		srv.Ingest = ingest
		ingest.Start()
		defer ingest.Close()
	}

	// Boot is done — flip ready so the desktop shell's /api/ready poll
	// unblocks and it can show its window.
	srv.SetReady(true)

	// Serve in a goroutine so the main goroutine can wait for a shutdown
	// signal and drain cleanly (Tauri sends SIGTERM/SIGINT on quit).
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s", *port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		log.Fatalf("server error: %v", err)
	case s := <-sig:
		log.Printf("received %s — shutting down gracefully", s)
		applog.Info("system", "shutting down")
		// Drain in-flight HTTP requests (bounded), then let the deferred
		// watcher/ingest/store closers run as main returns.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
		log.Printf("stopped")
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// defaultDataDir is the install root when nothing is configured: ~/.abookify
// (the desktop convention). Falls back to ./data if the home dir can't be
// resolved, so a bare `go run` in a sandbox still works.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "./data"
	}
	return filepath.Join(home, ".abookify")
}
