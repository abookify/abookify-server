package server

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
)

//go:embed static
var staticFiles embed.FS

type Server struct {
	store      *db.Store
	http       *http.Server
	Events     *EventBus
	Generator  *library.Generator
	rag        atomic.Pointer[llm.RAG]
	Ingest     *library.IngestQueue
	LibraryDir   string
	GeneratedDir string
	// BookNLPURL is the optional cast-of-characters service (EXPERIMENTAL).
	// Empty when the booknlp compose profile isn't running.
	BookNLPURL string
	// bn manages the BookNLP engine lifecycle (in-UI enable → server starts the
	// service; idle auto-stop). See booknlp_lifecycle.go.
	bn *bnManager

	// Version is the build version (stamped via -ldflags), surfaced on
	// /api/info + /api/ready so the desktop shell can show + update-check.
	Version string

	// ready flips true once the boot sequence (scan/migrate/link) is done.
	// GET /api/ready reports it; the desktop shell polls that before showing
	// its window. Reset to false on graceful shutdown so in-flight health
	// checks see the drain. atomic — read from HTTP goroutines.
	ready atomic.Bool

	// DataDir is the resolved root for the install (~/.abookify on desktop):
	// db, library, generated, models live under it. Surfaced on /api/setup
	// so the shell/UI can report where data lives + the models path.
	DataDir   string
	ModelsDir string

	// TTSURL/STTURL are the configured local-engine endpoints (empty when no
	// local engine is wired — e.g. an API-key-only install). Surfaced on
	// /api/setup + used by the model-download hooks to reach the engine.
	TTSURL string
	STTURL string

	// embedding dedupe — guards against re-entry when multiple STT jobs
	// finish back-to-back for the same work, or when ReloadLLM kicks off
	// a backfill while a per-work embed is already running.
	embedMu      sync.Mutex
	embedInFlight map[int64]bool

	// embed-all single-flight (#159b): library-change triggers (import/scan,
	// LLM-enable) coalesce into ONE running backfill instead of N concurrent
	// full passes. A trigger arriving mid-pass sets dirty so the runner loops
	// once more — catching works added after it swept past them.
	embedAllMu      sync.Mutex
	embedAllRunning bool

	// export-all is a big (multi-GB with audio) background sweep — single-flight
	// + a little progress state for a status readout.
	exportAllMu      sync.Mutex
	exportAllRunning bool
	exportAllDone    int
	exportAllTotal   int
	exportAllAudio   bool

	// serializes on-demand voice-preview generation (see voice_preview.go).
	voicePreviewMu sync.Mutex
	embedAllDirty   bool

	// alignment dedupe — same shape as embedInFlight; protects against two
	// post-STT auto-align goroutines racing on the same work.
	alignMu      sync.Mutex
	alignInFlight map[int64]bool
}

// RAG returns the current LLM RAG client, or nil when no LLM is
// configured. Safe for concurrent use; ReloadLLM swaps the pointer
// atomically so callers see either the old or the new client, never
// a torn read.
func (s *Server) RAG() *llm.RAG { return s.rag.Load() }

// ReloadLLM rebuilds the RAG client from the current settings + env
// fallbacks and swaps it into place. Called at startup and after every
// POST /api/settings so adding/changing an API key takes effect without
// a restart. Storing nil when no provider is configured is intentional —
// it disables Q&A handlers cleanly. On the nil→client transition it also
// kicks off a background embed backfill across all works (#159) so the
// first Q&A after enabling an LLM isn't degraded to keyword-only search.
func (s *Server) ReloadLLM() {
	settings, err := s.store.GetAllSettings()
	if err != nil {
		log.Printf("LLM reload: read settings failed: %v", err)
		return
	}

	provider := settings["llm_provider"]
	apiKey := settings["llm_api_key"]

	if provider == "" {
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			provider = "anthropic"
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		} else if os.Getenv("OPENAI_API_KEY") != "" {
			provider = "openai"
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}

	prev := s.rag.Load()

	if provider == "" || (provider != "ollama" && apiKey == "") {
		if prev != nil {
			log.Printf("LLM disabled (no provider/key configured)")
		}
		s.rag.Store(nil)
		return
	}

	client := llm.NewClient(llm.Provider(provider), apiKey, settings["llm_model"], settings["llm_base_url"])
	s.rag.Store(llm.NewRAG(s.store, client))
	log.Printf("LLM Q&A ready (provider: %s, model: %s)", provider, client.Model())

	if prev == nil {
		s.EmbedNewWorks() // backfill works added while no LLM was configured (#159)
	}
}

// OnJobUpdate is the Generator's job-update callback. It broadcasts the
// update to WebSocket subscribers and, on STT completion, kicks off a
// background chunk+embed for the work (#159) so a newly transcribed
// audiobook becomes searchable without a manual /api/works/{id}/embed.
func (s *Server) OnJobUpdate(job library.JobStatus) {
	s.Events.Broadcast(Event{Type: "job_update", Data: job})
	if job.Status == "completed" && job.WorkID > 0 {
		// Content-producing jobs change the work's exportable data
		// (transcript+sync for STT, audio+chapters for TTS), so bump
		// content_version for mobile's update-check. Embedding jobs are
		// excluded — embeddings are omitted from book.db, so the exported
		// slice is unchanged. The STT auto-align below may stamp again
		// (harmless) but this covers STT with no ebook peer to align to.
		switch job.Type {
		case "stt", "stt-redo", "tts":
			s.stampWork(job.WorkID)
		}
	}
	if job.Type == "stt" && job.Status == "completed" && job.WorkID > 0 {
		go s.EmbedWorkAsync(job.WorkID)
		go s.AlignWorkAsync(job.WorkID)
	}
}

// EmbedWorkAsync chunks (idempotent) and embeds every text book in the
// work. No-op when LLM isn't configured. Dedupes per-work so two
// concurrent triggers (e.g. STT completion + LLM-enable backfill) don't
// race the same chunks.
func (s *Server) EmbedWorkAsync(workID int64) {
	rag := s.RAG()
	if rag == nil {
		return
	}

	s.embedMu.Lock()
	if s.embedInFlight[workID] {
		s.embedMu.Unlock()
		return
	}
	s.embedInFlight[workID] = true
	s.embedMu.Unlock()

	defer func() {
		s.embedMu.Lock()
		delete(s.embedInFlight, workID)
		s.embedMu.Unlock()
	}()

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		return
	}
	for _, tf := range work.TextFiles {
		// Newly created transcript books (post-STT) won't be chunked yet.
		// ChunkBook short-circuits when chunks already exist.
		if err := library.ChunkBook(s.store, tf.ID); err != nil {
			log.Printf("embed: chunk book %d (%s): %v", tf.ID, tf.Filename, err)
			continue
		}
		n, err := rag.EmbedBook(tf.ID)
		if err != nil {
			log.Printf("embed: book %d (%s): %v", tf.ID, tf.Filename, err)
			continue
		}
		if n > 0 {
			log.Printf("embed: work %d book %d (%s): %d new chunks", workID, tf.ID, tf.Filename, n)
		}
	}
}

// AlignWorkAsync runs the anchor aligner on the work in the background.
// Triggered on STT completion so a newly-transcribed audiobook paired with
// an ebook gets its render-ready alignment row without a manual /align call.
// No-op when the work lacks either a transcript or an ebook peer (coverage
// 0, nil err). Logs to applog component "align" so failures land in the
// System Console without surfacing as STT errors.
func (s *Server) AlignWorkAsync(workID int64) {
	s.alignMu.Lock()
	if s.alignInFlight[workID] {
		s.alignMu.Unlock()
		return
	}
	s.alignInFlight[workID] = true
	s.alignMu.Unlock()
	defer func() {
		s.alignMu.Lock()
		delete(s.alignInFlight, workID)
		s.alignMu.Unlock()
	}()

	coverage, err := library.ComputeAnchorAlignment(s.store, workID)
	if err != nil {
		applog.Log(applog.LevelError, "align", "", workID, "auto-align failed",
			map[string]any{"error": err.Error(), "trigger": "stt-completed"})
		return
	}
	if coverage > 0 {
		applog.Log(applog.LevelInfo, "align", "", workID, "auto-align done",
			map[string]any{"coverage": coverage, "trigger": "stt-completed"})
		s.stampWork(workID)
	}
	// coverage==0 + nil err means the work has no ebook/transcript pair —
	// nothing to align, nothing worth logging.
}

// embedAllWorks runs EmbedWorkAsync for every work in the library. Each
// per-work embed is a no-op when its chunks already have embeddings, so this
// is cheap for the steady state and only does real work for newly-added books.
func (s *Server) embedAllWorks() {
	works, err := s.store.ListWorks()
	if err != nil {
		log.Printf("embed backfill: list works failed: %v", err)
		return
	}
	for _, w := range works {
		s.EmbedWorkAsync(w.ID)
	}
}

// EmbedNewWorks backfills embeddings for any work that needs them (#159/#159b)
// — fired when the library changes (import/scan) or the LLM is enabled, so a
// newly-added book becomes searchable without a restart or a manual embed.
// No-op when no LLM is configured. Single-flight + dirty-coalescing: overlapping
// triggers collapse into ONE running backfill, and one arriving mid-pass makes
// the runner loop once more so late-added works aren't missed. Idempotent —
// already-embedded works cost only a cheap "any unembedded chunks?" query.
func (s *Server) EmbedNewWorks() {
	if s.RAG() == nil {
		return
	}
	s.embedAllMu.Lock()
	if s.embedAllRunning {
		s.embedAllDirty = true
		s.embedAllMu.Unlock()
		return
	}
	s.embedAllRunning = true
	s.embedAllMu.Unlock()

	go func() {
		for {
			s.embedAllWorks()
			s.embedAllMu.Lock()
			if !s.embedAllDirty {
				s.embedAllRunning = false
				s.embedAllMu.Unlock()
				return
			}
			s.embedAllDirty = false // another trigger arrived mid-pass; sweep again
			s.embedAllMu.Unlock()
		}
	}()
}

func New(store *db.Store, port string) *Server {
	s := &Server{
		store:         store,
		Events:        NewEventBus(),
		embedInFlight: make(map[int64]bool),
		alignInFlight: make(map[int64]bool),
	}
	s.bn = newBNManager(s)

	// Drop any login tokens that expired while the server was down (#197).
	// Per-request validation also purges lazily; this keeps the table tidy.
	_ = store.PurgeExpiredAuthSessions()

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/ready", s.handleReady)
	mux.HandleFunc("GET /api/setup", s.handleSetup)
	mux.HandleFunc("GET /api/engines/status", s.handleEnginesStatus)
	mux.HandleFunc("POST /api/engines/install", s.handleEnginesInstall)
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/pair-qr", s.handlePairQR)
	mux.HandleFunc("GET /api/pair-payload", s.handlePairPayload)
	mux.HandleFunc("GET /api/server-info", s.handleServerInfo)
	mux.HandleFunc("POST /api/server-id/rotate", s.handleRotateServerID)
	mux.HandleFunc("GET /api/books", s.handleListBooks)
	mux.HandleFunc("GET /api/books/{id}", s.handleGetBook)
	mux.HandleFunc("GET /api/books/{id}/stream", s.handleStreamBook)
	mux.HandleFunc("GET /api/works", s.handleListWorks)
	mux.HandleFunc("GET /api/works/{id}", s.handleGetWork)
	mux.HandleFunc("GET /api/works/{id}/version", s.handleWorkVersion)
	mux.HandleFunc("GET /api/catalog", s.handleCatalog)
	mux.HandleFunc("GET /api/works/{id}/diff", s.handleWorkDiff)
	mux.HandleFunc("GET /api/works/{id}/coverage", s.handleWorkCoverage)
	mux.HandleFunc("GET /api/works/{id}/text-sync/{bookId}/{chapterIdx}", s.handleTextSync)
	mux.HandleFunc("GET /api/books/{bookId}/chapters/{idx}/summary", s.handleChapterSummary)
	mux.HandleFunc("GET /api/books/{bookId}/recap", s.handleBookRecap)
	mux.HandleFunc("GET /api/works/{id}/word-sync/{bookId}/{chapterIdx}", s.handleEbookWordSync)
	mux.HandleFunc("GET /api/works/{id}/cast", s.handleGetCast)
	mux.HandleFunc("POST /api/works/{id}/extract-cast", s.handleExtractCast)
	mux.HandleFunc("GET /api/booknlp/status", s.handleBookNLPStatus)
	mux.HandleFunc("POST /api/booknlp/enable", s.handleBookNLPEnable)
	mux.HandleFunc("POST /api/booknlp/disable", s.handleBookNLPDisable)
	mux.HandleFunc("GET /api/works/{id}/cover", s.handleWorkCover)
	mux.HandleFunc("POST /api/works/{id}/fetch-cover", s.handleFetchCover)
	mux.HandleFunc("POST /api/covers/fetch-missing", s.handleFetchMissingCovers)
	mux.HandleFunc("GET /api/books/{id}/chapters", s.handleListChapters)
	mux.HandleFunc("GET /api/books/{id}/chapters/{index}", s.handleGetChapter)
	mux.HandleFunc("GET /api/books/{id}/search", s.handleSearchBook)
	mux.HandleFunc("GET /api/works/{id}/search", s.handleSearchWork)
	mux.HandleFunc("GET /api/search", s.handleSearchLibrary)
	mux.HandleFunc("GET /api/books/{id}/waveform", s.handleWaveform)
	mux.HandleFunc("GET /api/works/{id}/waveform", s.handleWorkWaveform)
	mux.HandleFunc("POST /api/works/{id}/ask", s.handleAskQuestion)
	mux.HandleFunc("GET /api/works/{id}/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/works/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleListMessages)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handleAppendMessage)
	mux.HandleFunc("PUT /api/sessions/{id}", s.handleRenameSession)
	mux.HandleFunc("POST /api/sessions/{id}/scope", s.handleSetSessionScope)
	mux.HandleFunc("GET /api/works/{id}/qa-suggestions", s.handleQASuggestions)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/works/{id}/generate-audio", s.handleGenerateAudio)
	mux.HandleFunc("POST /api/works/{id}/transcribe", s.handleTranscribe)
	mux.HandleFunc("POST /api/works/{id}/detect-chapters", s.handleDetectChapters)
	mux.HandleFunc("POST /api/books/{id}/chapters", s.handleAddChapter)
	mux.HandleFunc("POST /api/works/{id}/align", s.handleForceAlign)
	mux.HandleFunc("POST /api/align-all", s.handleAlignAll)
	mux.HandleFunc("POST /api/works/{id}/embed", s.handleEmbed)
	mux.HandleFunc("POST /api/works/{id}/converse", s.handleConverse)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("GET /api/works/{id}/divergence", s.handleDivergence)
	mux.HandleFunc("POST /api/works/{id}/regenerate-chapter", s.handleRegenerateChapter)
	mux.HandleFunc("GET /api/works/{id}/sync/{audioBookId}/{chapterIdx}", s.handleGetSyncData)
	mux.HandleFunc("GET /api/works/{id}/alignments", s.handleListAlignments)
	mux.HandleFunc("PUT /api/works/{id}", s.handleUpdateWork)
	mux.HandleFunc("GET /api/works/duplicates", s.handleListDuplicates)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/works/{id}/merge", s.handleMergeWorks)
	mux.HandleFunc("DELETE /api/works/{id}", s.handleDeleteWork)
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleDeleteJob)
	mux.HandleFunc("GET /api/logs", s.handleListLogs)
	mux.HandleFunc("GET /api/queue/status", s.handleQueueStatus)
	mux.HandleFunc("DELETE /api/queue/failed/{name}", s.handleQueueRemoveFailed)
	mux.HandleFunc("POST /api/books/{id}/embed", s.handleEmbedBook)
	mux.HandleFunc("POST /api/works/{id}/reprocess", s.handleReprocessWork)
	mux.HandleFunc("GET /api/works/{id}/transcription-gaps", s.handleTranscriptionGaps)
	mux.HandleFunc("GET /api/transcription-gaps/summary", s.handleTranscriptionGapsSummary)
	mux.HandleFunc("POST /api/works/{id}/retry-stt", s.handleRetryTranscription)
	mux.HandleFunc("GET /api/works/{id}/position", s.handleGetPosition)
	mux.HandleFunc("POST /api/works/{id}/position", s.handleSavePosition)
	mux.HandleFunc("GET /api/tts/preview", s.handleTTSPreview)
	mux.HandleFunc("GET /api/tts/voices/{voice}/preview.mp3", s.handleVoicePreview)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("GET /api/settings/schema", s.handleSettingsSchema)
	mux.HandleFunc("POST /api/settings", s.handleSaveSettings)
	mux.HandleFunc("GET /api/llm/models", s.handleListLLMModels)
	mux.HandleFunc("POST /api/llm/test", s.handleTestLLM)
	mux.HandleFunc("GET /api/disk", s.handleDiskUsage)
	mux.HandleFunc("POST /api/library/rescan", s.handleLibraryRescan)
	mux.HandleFunc("GET /api/embeddings/coverage", s.handleEmbeddingsCoverage)
	mux.HandleFunc("POST /api/embeddings/refresh", s.handleEmbeddingsRefresh)
	mux.HandleFunc("GET /api/works/{id}/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("POST /api/works/{id}/bookmarks", s.handleCreateBookmark)
	mux.HandleFunc("PUT /api/bookmarks/{id}", s.handleUpdateBookmark)
	mux.HandleFunc("DELETE /api/bookmarks/{id}", s.handleDeleteBookmark)
	mux.HandleFunc("GET /api/works/{id}/export.abook", s.handleExportAbook)
	mux.HandleFunc("POST /api/export-all", s.handleExportAll)
	mux.HandleFunc("GET /api/export-all/status", s.handleExportAllStatus)
	mux.HandleFunc("GET /api/exports", s.handleListExports)
	mux.HandleFunc("GET /api/exports/{file}", s.handleGetExport)
	mux.HandleFunc("POST /api/import", s.handleImportAbook)
	mux.HandleFunc("POST /api/devices/register", s.handleRegisterDevice)
	mux.HandleFunc("GET /api/devices", s.handleListDevices)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("GET /api/ws", s.Events.HandleWS)

	// Serve sample audio files for quality comparison
	mux.Handle("GET /samples/", http.StripPrefix("/samples/",
		http.FileServer(http.Dir("/app/testdata/quality"))))

	// Serve embedded web UI
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /", http.FileServer(http.FS(staticFS)))

	s.http = &http.Server{
		Addr:    ":" + port,
		// auth is innermost so every request is still logged + CORS-
		// decorated, and OPTIONS preflight is handled by cors before it
		// reaches the gate (#197).
		Handler: accessLogMiddleware(corsMiddleware(s.authMiddleware(mux))),
	}

	return s
}

func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

// SetReady marks the server booted (or draining). GET /api/ready reflects it.
// On the boot→ready transition it kicks off a background pre-warm of the voice
// previews so the settings UI shows instant, pre-cached samples.
func (s *Server) SetReady(v bool) {
	s.ready.Store(v)
	if v {
		go s.prewarmVoicePreviews()
	}
}

// Shutdown gracefully drains the HTTP server: stop accepting new connections,
// let in-flight requests finish (bounded by ctx). Marks not-ready first so a
// polling shell/health check sees the drain. The caller closes the watcher,
// ingest queue, and DB after this returns.
func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	return s.http.Shutdown(ctx)
}

// handleReady is the lightweight readiness probe the desktop shell polls
// before showing its window (packaging-plan First Launch). Unlike /api/health
// it does NO upstream TTS/STT probing, so it's cheap to poll in a tight loop:
// 200 {"ready":true,...} once booted, 503 {"ready":false} while warming or
// draining. Connection-refused (process still starting) is the shell's "keep
// waiting" signal; the first 200 is "show the window".
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready := s.ready.Load()
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"ready":   ready,
		"version": s.Version,
	})
}

// handleHealth reports server liveness plus the reachability of the
// upstream TTS and STT services so the Settings page can show honest
// per-service status dots (the old version always lit both green off
// the server's own 200). Probes run in parallel and are bounded by
// probeTimeout so a hung Whisper doesn't stall the response.
//
// Shape:
//
//	{"status": "ok", "services": {"tts": "ok"|"down", "stt": "ok"|"down"}}
//
// A service is omitted from `services` when the client isn't wired up
// (e.g. no Generator). The top-level `status` always reflects the
// server itself, not the upstreams — callers that only need liveness
// (containers, the access-log filter) can keep ignoring `services`.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	const probeTimeout = 2 * time.Second
	services := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Probe a single upstream via its Health() method. Bounds the wait
	// with a select; the goroutine can leak briefly if the upstream
	// has a half-open TCP connection (the underlying http.Client has
	// no client-level timeout), but Go's transport defaults cap that
	// at the OS TCP timeout (~30-75s) — bounded and rare in practice.
	probe := func(name string, check func() error) {
		defer wg.Done()
		done := make(chan error, 1)
		go func() { done <- check() }()
		var status string
		select {
		case err := <-done:
			if err == nil {
				status = "ok"
			} else {
				status = "down"
			}
		case <-time.After(probeTimeout):
			status = "down"
		}
		mu.Lock()
		services[name] = status
		mu.Unlock()
	}

	if s.Generator != nil {
		if c := s.Generator.TTSClient(); c != nil {
			wg.Add(1)
			go probe("tts", c.Health)
		}
		if c := s.Generator.STTClient(); c != nil {
			wg.Add(1)
			go probe("stt", c.Health)
		}
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"services": services,
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "abookify",
		"version": version,
		"port":    s.http.Addr,
		"ready":   s.ready.Load(),
	})
}

func (s *Server) handleListBooks(w http.ResponseWriter, r *http.Request) {
	books, err := s.store.ListBooks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if books == nil {
		books = []db.Book{}
	}
	writeJSON(w, http.StatusOK, books)
}

func (s *Server) handleGetBook(w http.ResponseWriter, r *http.Request) {
	book, err := s.getBookByID(w, r)
	if err != nil || book == nil {
		return
	}
	writeJSON(w, http.StatusOK, book)
}

func (s *Server) handleStreamBook(w http.ResponseWriter, r *http.Request) {
	book, err := s.getBookByID(w, r)
	if err != nil || book == nil {
		return
	}

	f, err := os.Open(book.Path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found on disk"})
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot stat file"})
		return
	}

	contentType := "application/octet-stream"
	switch book.Format {
	case "mp3":
		contentType = "audio/mpeg"
	case "m4b", "m4a":
		contentType = "audio/mp4"
	case "flac":
		contentType = "audio/flac"
	case "aac":
		contentType = "audio/aac"
	case "opus":
		contentType = "audio/ogg"
	case "epub":
		contentType = "application/epub+zip"
	case "pdf":
		contentType = "application/pdf"
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, book.Filename, stat.ModTime(), f)
}

func (s *Server) handleListWorks(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if works == nil {
		works = []db.Work{}
	}
	// Enrich each work with its best alignment so the work-list UI can show
	// a coverage pill without an N+1 fetch. One SQL query for the whole library.
	best, _ := s.store.BestAlignmentByWork()
	type workWithAlign struct {
		db.Work
		Coverage        *float64 `json:"coverage,omitempty"`
		AlignmentMethod string   `json:"alignment_method,omitempty"`
	}
	out := make([]workWithAlign, len(works))
	for i, wk := range works {
		out[i].Work = wk
		if ba, ok := best[wk.ID]; ok {
			cov := ba.Confidence
			out[i].Coverage = &cov
			out[i].AlignmentMethod = ba.Method
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetWork(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	work, err := s.store.GetWork(id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	// Enrich with display-source hints so the UI knows which source to show.
	type workWithDisplay struct {
		*db.Work
		DisplayTextID  *int64 `json:"display_text_id,omitempty"`
		DisplayAudioID *int64 `json:"display_audio_id,omitempty"`
	}
	resp := workWithDisplay{Work: work}
	if dt := library.ResolveDisplayText(work); dt != nil {
		resp.DisplayTextID = &dt.ID
	}
	if da := library.ResolveDisplayAudio(work); da != nil {
		resp.DisplayAudioID = &da.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWorkVersion is the cheap update-check endpoint: mobile stores the
// versions it installed and polls this to decide whether to re-pull the
// .abook. See design/local-first-sync.md.
func (s *Server) handleWorkVersion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	schemaVersion, contentVersion, found, err := s.store.GetVersions(id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":  schemaVersion,
		"content_version": contentVersion,
	})
}

// handleCatalog returns library-listing summaries only — no chapters, content,
// or alignment pairs. This is the read shape that mirrors the device's
// catalog.db so the home screen never cracks open per-book detail.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	type catalogEntry struct {
		ID             int64    `json:"id"`
		Title          string   `json:"title"`
		Author         string   `json:"author"`
		Language       string   `json:"language"`
		CoverPath      string   `json:"cover_path"`
		HasAudio       bool     `json:"has_audio"`
		HasText        bool     `json:"has_text"`
		SourceKind     string   `json:"source_kind"`
		CoveragePct    *float64 `json:"coverage_pct"`
		AlignMethod    *string  `json:"align_method"`
		AlignUnit      *string  `json:"align_unit"`
		ContentVersion string   `json:"content_version"`
		SchemaVersion  int      `json:"schema_version"`
		UpdatedAt      string   `json:"updated_at"`
	}
	out := make([]catalogEntry, 0, len(works))
	for i := range works {
		wk := &works[i]
		sum := abook.SummarizeWork(s.store, wk)
		out = append(out, catalogEntry{
			ID:             wk.ID,
			Title:          wk.Title,
			Author:         wk.Author,
			Language:       "en",
			CoverPath:      fmt.Sprintf("/api/works/%d/cover", wk.ID),
			HasAudio:       wk.HasAudio,
			HasText:        wk.HasText,
			SourceKind:     sum.SourceKind,
			CoveragePct:    sum.CoveragePct,
			AlignMethod:    sum.AlignMethod,
			AlignUnit:      sum.AlignUnit,
			ContentVersion: wk.ContentVersion,
			SchemaVersion:  wk.SchemaVersion,
			UpdatedAt:      wk.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWorkDiff returns the render-ready source comparison for a work (#199):
// paired TEXT spans in reading order derived from the best word-level
// alignment. 404 when the work has no such cross-source alignment, so clients
// fall back to the offset-only AbookSummary view. Contract in SESSION_HANDOFF.
func (s *Server) handleWorkDiff(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	diff, found, err := library.BuildDiff(s.store, id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no cross-source alignment to compare"})
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleTextSync returns the reader follow-mode + per-paragraph audio time
// windows for a displayed text source + chapter (#210). The reader uses
// `mode` to choose word-by-word karaoke (transcript/word-anchor, driven by
// sync_data) vs paragraph-level follow (embedding/paragraph) and, for the
// latter, highlights the paragraph whose window contains the current audio
// time. Always 200 — `mode:"none"` / empty `spans` degrade gracefully.
func (s *Server) handleTextSync(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	bookID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("bookId")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bookId"})
		return
	}
	chapterIdx, err := strconv.Atoi(strings.TrimSpace(r.PathValue("chapterIdx")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chapterIdx"})
		return
	}
	ts, err := library.BuildTextSync(s.store, id, bookID, chapterIdx)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// handleEbookWordSync returns a composed per-word audio map for one chapter of
// a word-anchor-aligned ebook (#210b) — the same {w,s,e} shape as the
// transcript sync_data, so the reader drives ebook word-by-word karaoke
// through the identical path. `[]` (not 404) when the source isn't a
// word-anchor ebook or the chapter has no per-word timing, so the client
// cleanly falls back to paragraph-follow.
func (s *Server) handleEbookWordSync(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	bookID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("bookId")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bookId"})
		return
	}
	chapterIdx, err := strconv.Atoi(strings.TrimSpace(r.PathValue("chapterIdx")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chapterIdx"})
		return
	}
	words, err := library.BuildEbookWordSync(s.store, id, bookID, chapterIdx)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if words == nil {
		words = []library.SyncWord{}
	}
	writeJSON(w, http.StatusOK, words)
}

// handleWorkCoverage returns per-source-pair DIRECTIONAL coverage (#199): for
// each ebook↔transcript pair, both the audio→ebook (quality) and ebook→audio
// (scope) ratios plus the raw word counts. No span detail (that's /diff). The
// pairs list is the column-pair contract the #200 readout + mobile #201 build
// against — see engineering/handoff/server-web.md. Always 200 with a (possibly
// empty) pairs array so consumers degrade gracefully.
func (s *Server) handleWorkCoverage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	cov, err := library.BuildCoverage(s.store, id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cov)
}

// handleGetCast returns a work's cast of characters (EXPERIMENTAL). Always
// includes experimental:true and enabled (the feature flag) so the UI can
// render the mandatory badge and decide whether to offer extraction. An
// absent cast is an empty list, not an error — consumers degrade gracefully.
func (s *Server) handleGetCast(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	chars, err := s.store.ListCharactersForWork(id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if chars == nil {
		chars = []db.Character{}
	}
	settings, _ := s.store.GetAllSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"experimental": true,
		"enabled":      settings["booknlp_enabled"] == "true" && s.BookNLPURL != "",
		"characters":   chars,
	})
}

// handleExtractCast runs the booknlp service over the work's EPUB and stores
// the cast. Gated behind the booknlp_enabled feature flag + a configured
// service URL. Synchronous: BookNLP takes minutes, so the client should show
// a spinner. EXPERIMENTAL.
func (s *Server) handleExtractCast(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	settings, _ := s.store.GetAllSettings()
	if settings["booknlp_enabled"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Cast extraction is off. Turn it on with the Enable button in the cast panel."})
		return
	}
	if s.BookNLPURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The cast engine isn't available on this server."})
		return
	}
	n, err := library.ExtractCast(s.store, s.BookNLPURL, id)
	if err != nil {
		// The engine is opt-in and may be stopped (idle auto-stop). Fail SOFT:
		// kick off a start in the background and tell the user to retry — never
		// surface a docker command.
		if errors.Is(err, library.ErrBookNLPUnreachable) {
			applog.Log(applog.LevelWarn, "booknlp", "", id, "cast extraction — engine not running, starting it",
				map[string]any{"error": err.Error()})
			go s.bn.enable() // idempotent: (re)start the stopped engine
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "The cast engine is starting up (the first run downloads it). Give it a moment and try again.",
			})
			return
		}
		// A foreseeable input condition (no EPUB / text not extracted yet) is a
		// 422, not a server error — still graceful, never a bare 500.
		if errors.Is(err, library.ErrNoCastableText) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		applog.Log(applog.LevelError, "booknlp", "", id, "cast extraction failed",
			map[string]any{"error": err.Error()})
		writeServerError(w, r, err)
		return
	}
	s.bn.touch() // successful extraction — keep the engine warm + re-arm idle-stop
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"characters": n})
}

// handleBookNLPStatus reports the cast-engine lifecycle state for the in-UI
// enable flow (replaces surfacing a docker command).
func (s *Server) handleBookNLPStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.bn.status())
}

// handleBookNLPEnable turns the experimental cast engine on and starts it
// (async; the server drives the compose profile). Auth-gated like all /api.
func (s *Server) handleBookNLPEnable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.bn.enable())
}

// handleBookNLPDisable stops the cast engine and clears the feature flag.
func (s *Server) handleBookNLPDisable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.bn.disable())
}

// stampWork records that a work's exportable data just changed, bumping its
// content_version (and re-stamping schema_version to the current book.db
// shape) so mobile's update-check sees it. Best-effort: a stamp failure is
// logged but never fails the originating request.
func (s *Server) stampWork(workID int64) {
	if err := s.store.StampVersions(workID, abook.BookDBSchemaVersion); err != nil {
		applog.Log(applog.LevelWarn, "server", "", workID, "version stamp failed",
			map[string]any{"error": err.Error()})
	}
}

func (s *Server) handleListChapters(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	chapters, err := s.store.ListChapters(id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if chapters == nil {
		chapters = []db.Chapter{}
	}
	writeJSON(w, http.StatusOK, chapters)
}

func (s *Server) handleSearchBook(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q parameter"})
		return
	}

	chunks, err := s.store.SearchChunks(id, query)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if chunks == nil {
		chunks = []db.Chunk{}
	}
	writeJSON(w, http.StatusOK, chunks)
}

func (s *Server) handleGetChapter(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	indexStr := strings.TrimSpace(r.PathValue("index"))
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chapter index"})
		return
	}

	ch, err := s.store.GetChapterContent(id, index)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if ch == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chapter not found"})
		return
	}

	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) handleTTSPreview(w http.ResponseWriter, r *http.Request) {
	voice := r.URL.Query().Get("voice")
	if voice == "" {
		voice = "af_heart"
	}
	if s.Generator == nil || s.Generator.TTSClient() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TTS service not available"})
		return
	}
	audio, err := s.Generator.TTSClient().Synthesize(
		"Here is a preview of this voice. Once upon a time, in a quiet little village nestled between rolling hills, there lived a curious young reader.",
		voice,
	)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(audio)
}

func (s *Server) handleMergeWorks(w http.ResponseWriter, r *http.Request) {
	targetID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		SourceID int64 `json:"source_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SourceID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_id required"})
		return
	}
	if targetID == req.SourceID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot merge a work into itself"})
		return
	}
	if err := s.store.MergeWorks(targetID, req.SourceID); err != nil {
		writeServerError(w, r, err)
		return
	}
	log.Printf("merged work %d into work %d", req.SourceID, targetID)
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "merged"})
}

func (s *Server) handleDeleteWork(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeleteWork(id); err != nil {
		writeServerError(w, r, err)
		return
	}
	log.Printf("deleted work %d", id)
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGenerateAudio(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TTS service not configured"})
		return
	}

	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}

	if !work.HasText || len(work.TextFiles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no text files to generate audio from"})
		return
	}

	// Parse optional voice from request body
	var req struct {
		Voice string `json:"voice"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	textBookID := work.TextFiles[0].ID
	jobID, started := s.Generator.GenerateAudioFromText(workID, textBookID, req.Voice)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "job already running", "job_id": jobID})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (s *Server) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "STT service not configured"})
		return
	}

	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}

	if !work.HasAudio {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no audio files to transcribe"})
		return
	}

	jobID, started := s.Generator.TranscribeAudio(workID)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "job already running", "job_id": jobID})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

// POST /api/works/{id}/retry-stt — re-transcribe specific files (by
// basename) and merge into the existing sidecar. Used by the
// gap-detection UI to fix Whisper failures without leaving the page.
//
// Body: {"filenames": ["07.mp3", "13.mp3"]}
// Returns 202 with job_id, or 409 if a redo is already running.
func (s *Server) handleRetryTranscription(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "STT service not configured"})
		return
	}
	workID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var body struct {
		Filenames []string `json:"filenames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if len(body.Filenames) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "filenames required"})
		return
	}
	jobID, started := s.Generator.RetryTranscriptionForFiles(workID, body.Filenames)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "job already running", "job_id": jobID})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusOK, []library.JobStatus{})
		return
	}
	writeJSON(w, http.StatusOK, s.Generator.GetJobs())
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id := r.PathValue("id")
	job := s.Generator.GetJob(id)
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleRegenerateChapter(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TTS not configured"})
		return
	}

	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var req struct {
		BookID      int64  `json:"book_id"`
		ChapterIdx  int    `json:"chapter_idx"`
		Voice       string `json:"voice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}

	// Find the text book
	bookID := req.BookID
	if bookID == 0 && len(work.TextFiles) > 0 {
		bookID = work.TextFiles[0].ID
	}

	ch, err := s.store.GetChapterContent(bookID, req.ChapterIdx)
	if err != nil || ch == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chapter not found"})
		return
	}

	voice := req.Voice
	if voice == "" || strings.HasPrefix(voice, "en_US") {
		voice = "af_heart"
	}

	jobID, started := s.Generator.RegenerateChapter(workID, bookID, ch, voice)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already running", "job_id": jobID})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}

func (s *Server) handleWorkCover(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	coverPath := fmt.Sprintf("%s/covers/work-%d.jpg", s.LibraryDir, id)
	if _, err := os.Stat(coverPath); err != nil {
		http.Error(w, "no cover", http.StatusNotFound)
		return
	}

	http.ServeFile(w, r, coverPath)
}

func (s *Server) handleFetchCover(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	coversDir := s.LibraryDir + "/covers"
	path := library.FetchCoverFromOpenLibrary(work.Title, work.Author, coversDir, workID)
	if path == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no cover found on OpenLibrary"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "path": path})
}

// handleFetchMissingCovers backfills cover art from OpenLibrary for every work
// that has no cover file yet. Sequential + best-effort (one work's failure
// doesn't stop the rest); OpenLibrary won't resolve junk-title/no-author works
// and those just count as "not found". User-triggered from Settings.
func (s *Server) handleFetchMissingCovers(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	coversDir := filepath.Join(s.LibraryDir, "covers")
	// First delete any corrupt/truncated covers so the backfill below refetches
	// them (fixes half-drawn covers from earlier partial downloads).
	_, purged := library.SweepCorruptCovers(coversDir)
	var fetched, missing, skipped int
	for i := range works {
		wk := &works[i]
		coverFile := filepath.Join(coversDir, fmt.Sprintf("work-%d.jpg", wk.ID))
		if fi, err := os.Stat(coverFile); err == nil && fi.Size() > 0 {
			continue // already has a (valid — corrupt ones were just purged) cover
		}
		if strings.TrimSpace(wk.Title) == "" {
			skipped++
			continue
		}
		if library.FetchCoverFromOpenLibrary(wk.Title, wk.Author, coversDir, wk.ID) != "" {
			fetched++
		} else {
			missing++
		}
	}
	applog.Info("server", fmt.Sprintf("fetch-missing-covers: purged_corrupt=%d fetched=%d not_found=%d skipped=%d", purged, fetched, missing, skipped))
	if (fetched > 0 || purged > 0) && s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged_corrupt": purged, "fetched": fetched, "not_found": missing, "skipped": skipped})
}

func (s *Server) handleAskQuestion(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No LLM configured. Add an API key in Settings.",
		})
		return
	}

	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var req struct {
		Question string              `json:"question"`
		Scope    library.QueryScope `json:"scope,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing question"})
		return
	}

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}

	if !work.HasText || len(work.TextFiles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no text content to search"})
		return
	}

	// New path: vector search + alignment-aware citations with audio times.
	// Falls back gracefully when embeddings aren't populated.
	answer, err := library.AskWithCitations(s.store, rag, workID, req.Question, req.Scope)
	if err != nil {
		// Legacy fallback: keyword-only search on the first text file
		legacy, err2 := rag.Ask(work.TextFiles[0].ID, req.Question, work.Title)
		if err2 != nil {
			writeServerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, legacy)
		return
	}

	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) handleListBookmarks(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	bookmarks, err := s.store.ListBookmarks(workID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if bookmarks == nil {
		bookmarks = []db.Bookmark{}
	}
	writeJSON(w, http.StatusOK, bookmarks)
}

func (s *Server) handleCreateBookmark(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var bm db.Bookmark
	if err := json.NewDecoder(r.Body).Decode(&bm); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	bm.WorkID = workID
	id, err := s.store.CreateBookmark(bm)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleUpdateBookmark(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Note  string `json:"note"`
		Color string `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.store.UpdateBookmark(id, req.Note, req.Color); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteBookmark(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.store.DeleteBookmark(id); err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			days = n
		}
	}
	daily, _ := s.store.PlaybackStatsByDay(days)
	total, _ := s.store.PlaybackTotalSeconds()
	works, _ := s.store.ListWorks()
	writeJSON(w, http.StatusOK, map[string]any{
		"total_listening_seconds": total,
		"total_listening_hours":   total / 3600,
		"total_works":             len(works),
		"daily":                   daily,
	})
}

func (s *Server) handleListDuplicates(w http.ResponseWriter, r *http.Request) {
	groups, err := library.FindDuplicateWorks(s.store)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) handleUpdateWork(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	// All three fields are optional. *int64 lets the client send
	// display_text_book_id:0 to clear the override (a plain int64
	// couldn't distinguish "clear" from "field absent").
	var req struct {
		Title             string `json:"title"`
		Author            string `json:"author"`
		DisplayTextBookID *int64 `json:"display_text_book_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if req.Title != "" || req.Author != "" {
		if err := s.store.UpdateWork(workID, req.Title, req.Author); err != nil {
			writeServerError(w, r, err)
			return
		}
	}
	if req.DisplayTextBookID != nil {
		bookID := *req.DisplayTextBookID
		// Verify the book actually belongs to this work and isn't an
		// internal pipeline source. 0 always allowed (clears override).
		if bookID != 0 {
			work, err := s.store.GetWork(workID)
			if err != nil || work == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
				return
			}
			ok := false
			for _, tf := range work.TextFiles {
				if tf.ID == bookID && tf.Visibility != "internal" {
					ok = true
					break
				}
			}
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "book is not a visible text source on this work"})
				return
			}
		}
		if err := s.store.SetDisplayTextBook(workID, bookID); err != nil {
			writeServerError(w, r, err)
			return
		}
	}
	s.stampWork(workID)
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAddChapter(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Title    string  `json:"title"`
		StartSec float64 `json:"start_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing title"})
		return
	}
	// Find the next available chapter index for this book.
	chapters, _ := s.store.ListChapters(bookID)
	nextIdx := len(chapters)
	ch := db.Chapter{
		BookID:     bookID,
		Index:      nextIdx,
		Title:      req.Title,
		Src:        "user",
		StartSec:   req.StartSec,
		Confidence: 1.0,
	}
	if err := s.store.InsertChapter(ch); err != nil {
		writeServerError(w, r, err)
		return
	}
	if bk, _ := s.store.GetBook(bookID); bk != nil && bk.WorkID > 0 {
		s.stampWork(bk.WorkID)
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) handleWaveform(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	book, err := s.store.GetBook(bookID)
	if err != nil || book == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
		return
	}
	genDir := s.GeneratedDir
	if genDir == "" {
		genDir = "/generated"
	}
	wf, err := library.GenerateWaveform(*book, genDir)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

// handleWorkWaveform returns one waveform spanning every audio file in
// the work, re-bucketed onto a single timeline. Lets the mobile mini
// player and full-screen scrubber render a peak shape that matches the
// book-global time text instead of the current track's local time (#180).
func (s *Server) handleWorkWaveform(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	if !work.HasAudio {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "work has no audio"})
		return
	}
	genDir := s.GeneratedDir
	if genDir == "" {
		genDir = "/generated"
	}
	wf, err := library.GenerateWorkWaveform(*work, genDir)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) handleSearchWork(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q parameter"})
		return
	}
	hits, err := library.SearchWork(s.store, workID, query, 20)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// handleSearchLibrary searches chapter content across every work and
// returns up to `limit` passage hits with work + chapter identity so
// the toolbar's library-wide search can route a click to the right
// reader + position.
func (s *Server) handleSearchLibrary(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q parameter"})
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	hits, err := library.SearchLibrary(s.store, query, limit)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if hits == nil {
		hits = []library.LibraryHit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *Server) handleListAlignments(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	alignments, err := s.store.ListAlignmentsForWork(workID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, alignments)
}

func (s *Server) handleDivergence(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	report, err := library.ComputeDivergence(s.store, workID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if report == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no alignment available"})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleConverse: voice conversation round-trip. Expects multipart/form-data
// with "audio" field (user's recorded question) and optional "voice" field
// for the answer TTS. Returns transcribed question, generated answer,
// citations, and TTS audio (base64-encoded mp3).
// handleUpload accepts multipart file uploads and saves them to the library
// directory under an "imports/" subfolder. After saving, triggers a library
// rescan. Used by the drag-and-drop import in the web UI.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "library path not configured"})
		return
	}
	if err := r.ParseMultipartForm(512 << 20); err != nil { // 512MB cap
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse form: " + err.Error()})
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files provided"})
		return
	}
	importDir := s.LibraryDir + "/imports"
	if err := os.MkdirAll(importDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create imports dir: " + err.Error()})
		return
	}
	var saved []string
	for _, fh := range files {
		src, err := fh.Open()
		if err != nil {
			continue
		}
		destPath := importDir + "/" + fh.Filename
		dst, err := os.Create(destPath)
		if err != nil {
			src.Close()
			continue
		}
		io.Copy(dst, src)
		dst.Close()
		src.Close()
		saved = append(saved, fh.Filename)
		log.Printf("upload: saved %s (%d bytes)", fh.Filename, fh.Size)
	}
	// Trigger a library rescan in the background — same path the
	// Settings "Rescan now" button uses, so MOBI conversion + sidecar
	// import + chapter extraction all run on uploaded files for free.
	go func() {
		result, err := Rescan(s.store, s.LibraryDir)
		if err != nil {
			log.Printf("upload: rescan failed: %v", err)
			return
		}
		log.Printf("upload: rescan complete — %d new/changed, %d new works", result.Scanned, result.NewWorks)
		if s.Events != nil {
			s.Events.Broadcast(Event{Type: "library_updated"})
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": len(saved), "files": saved})
}

func (s *Server) handleConverse(w http.ResponseWriter, r *http.Request) {
	if s.Generator == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "generation engine not available"})
		return
	}
	rag := s.RAG()
	if rag == nil || rag.Client() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM not configured"})
		return
	}
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32MB cap
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse form: " + err.Error()})
		return
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing audio file"})
		return
	}
	defer file.Close()
	voice := r.FormValue("voice")
	if voice == "" {
		voice = "af_heart"
	}
	// Detect extension from filename.
	ext := "webm"
	if header != nil {
		if n := header.Filename; n != "" {
			if dot := strings.LastIndex(n, "."); dot >= 0 {
				ext = n[dot+1:]
			}
		}
	}
	audioBytes, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read upload: " + err.Error()})
		return
	}
	tmpPath, err := library.SaveUploadedAudio(audioBytes, ext)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save upload: " + err.Error()})
		return
	}
	defer os.Remove(tmpPath)

	resp, err := library.Converse(s.store, s.Generator.STTClient(), s.Generator.TTSClient(), rag, workID, tmpPath, voice)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil || rag.Client() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM not configured — add OpenAI API key in Settings"})
		return
	}
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	totalEmbedded := 0
	for _, tf := range work.TextFiles {
		n, err := library.EmbedChunksForBook(s.store, rag.Client(), tf.ID)
		if err != nil {
			writeServerError(w, r, err)
			return
		}
		totalEmbedded += n
	}
	writeJSON(w, http.StatusOK, map[string]any{"chunks_embedded": totalEmbedded})
}

func (s *Server) handleForceAlign(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	coverage, err := library.ComputeAnchorAlignment(s.store, workID)
	if err != nil {
		applog.Log(applog.LevelError, "align", "", workID, "anchor align failed",
			map[string]any{"error": err.Error()})
		writeServerError(w, r, err)
		return
	}
	applog.Log(applog.LevelInfo, "align", "", workID, "anchor align done",
		map[string]any{"coverage": coverage})
	s.stampWork(workID)
	writeJSON(w, http.StatusOK, map[string]any{
		"method":   "anchor",
		"coverage": coverage,
	})
}

// handleAlignAll backfills anchor alignment over the whole library. Useful
// when an ebook is added to a work that was transcribed before the
// post-STT auto-align hook existed (or before the work had an ebook peer).
// Synchronous so the UI can show a one-shot summary; each work is
// sub-second when chunks are cached.
func (s *Server) handleAlignAll(w http.ResponseWriter, r *http.Request) {
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	applog.Info("align", fmt.Sprintf("align-all backfill starting (%d works)", len(works)))

	var aligned, skipped, errored int
	var totalCov float64
	for _, wk := range works {
		cov, err := library.ComputeAnchorAlignment(s.store, wk.ID)
		if err != nil {
			errored++
			applog.Log(applog.LevelError, "align", "", wk.ID, "align-all: work failed",
				map[string]any{"error": err.Error()})
			continue
		}
		if cov == 0 {
			skipped++ // no transcript or no ebook peer
			continue
		}
		aligned++
		totalCov += cov
		s.stampWork(wk.ID)
		applog.Log(applog.LevelInfo, "align", "", wk.ID, "align-all: work done",
			map[string]any{"coverage": cov})
	}
	var avg float64
	if aligned > 0 {
		avg = totalCov / float64(aligned)
	}
	applog.Info("align", fmt.Sprintf("align-all backfill done: aligned=%d skipped=%d errored=%d avg=%.3f",
		aligned, skipped, errored, avg))
	writeJSON(w, http.StatusOK, map[string]any{
		"total":            len(works),
		"aligned":          aligned,
		"skipped":          skipped,
		"errored":          errored,
		"average_coverage": avg,
	})
}

func (s *Server) handleDetectChapters(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	n, err := library.DetectChaptersFromStoredSync(s.store, workID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if n > 0 {
		s.stampWork(workID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"chapters_detected": n})
}

func (s *Server) handleGetSyncData(w http.ResponseWriter, r *http.Request) {
	workID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	audioBookID, _ := strconv.ParseInt(r.PathValue("audioBookId"), 10, 64)
	chapterIdx, _ := strconv.Atoi(r.PathValue("chapterIdx"))

	data, err := s.store.GetSyncData(workID, audioBookID, chapterIdx)
	if err != nil {
		writeServerError(w, r, err)
		return
	}

	// Return raw JSON array directly
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(data))
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "inactive" {
		// Delete all non-running jobs
		jobs, _ := s.store.ListJobs()
		for _, j := range jobs {
			if j.Status != "running" && j.Status != "queued" {
				s.store.DeleteJob(j.ID)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
		return
	}
	s.store.DeleteJob(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetPosition(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	pos, err := s.store.GetPosition(workID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if pos == nil {
		writeJSON(w, http.StatusOK, map[string]any{"work_id": workID, "position_secs": 0, "file_index": 0})
		return
	}
	writeJSON(w, http.StatusOK, pos)
}

func (s *Server) handleSavePosition(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var pos db.PlaybackPosition
	if err := json.NewDecoder(r.Body).Decode(&pos); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	pos.WorkID = workID
	if err := s.store.SavePosition(pos); err != nil {
		writeServerError(w, r, err)
		return
	}
	// Record a 10-second listening event for analytics. The web/mobile
	// clients save position every 10 seconds while playing — each save
	// represents ~10s of actual listening time.
	s.store.RecordPlayback(workID, "listen", 10)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// abookBaseName builds a meaningful, filesystem-safe base name (no extension)
// for a work's .abook: "<Title> - <Author>", falling back to "work-<id>" when
// the title is empty. Used by the single-work download + the export-all set so
// served files are self-describing (not "work-22.abook").
func abookBaseName(title, author string, workID int64) string {
	name := strings.TrimSpace(title)
	if a := strings.TrimSpace(author); a != "" {
		name += " - " + a
	}
	name = sanitizeFilename(name)
	if name == "" {
		return fmt.Sprintf("work-%d", workID)
	}
	return name
}

// sanitizeFilename strips characters unsafe on Windows/macOS/Linux, collapses
// whitespace, drops leading/trailing dots+spaces (Windows), and caps length.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20: // control chars
			b.WriteRune(' ')
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ") // collapse whitespace runs
	out = strings.Trim(out, " .")
	if len(out) > 120 {
		out = strings.Trim(out[:120], " .")
	}
	return out
}

func (s *Server) handleExportAbook(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	workID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	work, err := s.store.GetWork(workID)
	if err != nil || work == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}

	// Create temp file for the export
	tmpFile, err := os.CreateTemp("", "abook-export-*.abook")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create temp file"})
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// audio=0 produces a lightweight container (book.db + manifest + cover,
	// no bundled audio) — shareable/inspectable, audio streams from the
	// server. Default bundles audio for a self-contained offline copy.
	includeAudio := r.URL.Query().Get("audio") != "0"
	// Carry chunk embeddings so a downloaded .abook can do on-device cosine
	// retrieval offline (mobile semantic Q&A). Backward-compatible — older
	// clients ignore the column. See the size note in ExportOptions.
	if err := abook.ExportV2(s.store, work, tmpPath, s.LibraryDir, abook.ExportOptions{IncludeAudio: includeAudio, IncludeEmbeddings: true}); err != nil {
		writeServerError(w, r, err)
		return
	}

	safeName := abookBaseName(work.Title, work.Author, work.ID)

	w.Header().Set("Content-Type", "application/x-abook+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.abook"`, safeName))
	http.ServeFile(w, r, tmpPath)
}

// handleExportAll regenerates the derived v2 .abook set for every aligned work
// into {libraryDir}/exports/<Title> - <Author>.abook (metadata-derived,
// filesystem-safe; collisions get a " (N)" suffix). Audio + the original ebook
// bundle BY DEFAULT — book.db's alignment is edition-locked to one exact
// recording + edition, so a .abook without its audio isn't portable. This makes
// the set multi-GB (accepted). ?audio=0 (or ?lite=1) produces a small text-only
// set. Because a full sweep with audio is large + slow, it runs in the
// BACKGROUND (single-flight); the handler returns 202 immediately and progress
// is available at GET /api/export-all/status.
func (s *Server) handleExportAll(w http.ResponseWriter, r *http.Request) {
	includeAudio := r.URL.Query().Get("audio") != "0" && r.URL.Query().Get("lite") != "1"

	s.exportAllMu.Lock()
	if s.exportAllRunning {
		s.exportAllMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "export-all is already running"})
		return
	}
	s.exportAllRunning = true
	s.exportAllDone, s.exportAllTotal, s.exportAllAudio = 0, 0, includeAudio
	s.exportAllMu.Unlock()

	go func() {
		defer func() {
			s.exportAllMu.Lock()
			s.exportAllRunning = false
			s.exportAllMu.Unlock()
		}()
		s.runExportAll(includeAudio)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":        "started",
		"include_audio": includeAudio,
		"note":          "running in the background — poll GET /api/export-all/status",
	})
}

// handleExportAllStatus reports the background sweep's progress.
func (s *Server) handleExportAllStatus(w http.ResponseWriter, r *http.Request) {
	s.exportAllMu.Lock()
	defer s.exportAllMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"running":       s.exportAllRunning,
		"done":          s.exportAllDone,
		"total":         s.exportAllTotal,
		"include_audio": s.exportAllAudio,
	})
}

// runExportAll performs the sweep (called in a goroutine by handleExportAll).
func (s *Server) runExportAll(includeAudio bool) {
	works, err := s.store.ListWorks()
	if err != nil {
		applog.Log(applog.LevelError, "server", "", 0, "export-all: list works failed", map[string]any{"error": err.Error()})
		return
	}
	exportDir := filepath.Join(s.LibraryDir, "exports")
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		applog.Log(applog.LevelError, "server", "", 0, "export-all: mkdir failed", map[string]any{"error": err.Error()})
		return
	}
	// Clear the previous set first so renamed/removed works don't leave stale
	// files behind (filenames are metadata-derived, not work-<id>).
	if old, err := os.ReadDir(exportDir); err == nil {
		for _, e := range old {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".abook") {
				os.Remove(filepath.Join(exportDir, e.Name()))
			}
		}
	}

	// Pre-count the aligned works for a progress denominator.
	var candidates []*db.Work
	for i := range works {
		if abook.SummarizeWork(s.store, &works[i]).SourceKind == "aligned" {
			candidates = append(candidates, &works[i])
		}
	}
	s.exportAllMu.Lock()
	s.exportAllTotal = len(candidates)
	s.exportAllMu.Unlock()

	usedNames := map[string]bool{}
	var exported, failed int
	for _, wk := range candidates {
		full, err := s.store.GetWork(wk.ID)
		if err != nil || full == nil {
			failed++
			continue
		}
		base := abookBaseName(wk.Title, wk.Author, wk.ID)
		name := base
		for n := 2; usedNames[name]; n++ {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		usedNames[name] = true
		out := filepath.Join(exportDir, name+".abook")
		if err := abook.ExportV2(s.store, full, out, s.LibraryDir, abook.ExportOptions{IncludeAudio: includeAudio, IncludeEmbeddings: true}); err != nil {
			applog.Log(applog.LevelError, "server", "", wk.ID, "export-all: work failed", map[string]any{"error": err.Error()})
			failed++
			continue
		}
		exported++
		s.exportAllMu.Lock()
		s.exportAllDone = exported
		s.exportAllMu.Unlock()
	}
	applog.Info("server", fmt.Sprintf("export-all: exported=%d failed=%d audio=%v dir=%s", exported, failed, includeAudio, exportDir))
}

// handleListExports lists the pre-generated v2 .abook set written by
// export-all to {libraryDir}/exports/. Each entry carries the manifest's
// identity + version stamps + on-disk size so a client can decide what to
// pull. Returns an empty array when the dir is absent (export-all not run).
func (s *Server) handleListExports(w http.ResponseWriter, r *http.Request) {
	exportDir := filepath.Join(s.LibraryDir, "exports")
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		// No exports yet (dir missing) is not an error — return empty.
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type exportEntry struct {
		File           string   `json:"file"`
		WorkID         int64    `json:"work_id"`
		Title          string   `json:"title"`
		Author         string   `json:"author"`
		SourceKind     string   `json:"source_kind"`
		SchemaVersion  int      `json:"schema_version"`
		ContentVersion string   `json:"content_version"`
		CoveragePct    *float64 `json:"coverage_pct"`
		SizeBytes      int64    `json:"size_bytes"`
	}
	out := []exportEntry{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".abook") {
			continue
		}
		full := filepath.Join(exportDir, e.Name())
		entry := exportEntry{File: e.Name()}
		if info, err := e.Info(); err == nil {
			entry.SizeBytes = info.Size()
		}
		if m, err := abook.ReadManifest(full); err == nil {
			entry.WorkID = m.WorkID
			entry.Title = m.Title
			entry.Author = m.Author
			entry.SourceKind = m.SourceKind
			entry.SchemaVersion = m.SchemaVersion
			entry.ContentVersion = m.ContentVersion
			entry.CoveragePct = m.CoveragePct
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetExport serves a single file from the exports dir. The {file} path
// value is reduced to its base name so it can't escape the directory.
func (s *Server) handleGetExport(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	if name == "." || name == "/" || !strings.HasSuffix(name, ".abook") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid file"})
		return
	}
	path := filepath.Join(s.LibraryDir, "exports", name)
	if _, err := os.Stat(path); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "application/x-abook+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, path)
}

// handleImportAbook accepts a .abook upload (multipart "file") from an authed
// client — the reverse of download — and imports it as a work. It dedupes vs
// existing works by title+author using content_version: on a match it returns a
// 409 conflict (with which side is newer) for the client to resolve, unless the
// caller passes ?on_conflict=replace|skip|new.
func (s *Server) handleImportAbook(w http.ResponseWriter, r *http.Request) {
	// A .abook is multi-GB when audio is bundled (edition-locked), so allow a
	// generous upload from an authed device.
	r.Body = http.MaxBytesReader(w, r.Body, 20<<30) // 20 GB

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file"})
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".abook") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file must have .abook extension"})
		return
	}

	tmpFile, err := os.CreateTemp("", "abook-import-*.abook")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create temp file"})
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save upload"})
		return
	}
	tmpFile.Close()

	// Read identity up front to dedupe.
	manifest, err := abook.ReadManifest(tmpPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a valid .abook: " + err.Error()})
		return
	}
	onConflict := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("on_conflict"))) // prompt(default)|replace|skip|new

	existingID, existingCV, exists, err := s.store.FindWorkByTitleAuthor(manifest.Title, manifest.Author)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	replaced := false
	if exists && onConflict != "new" {
		switch onConflict {
		case "skip":
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "skipped", "reason": "a work with this title/author already exists",
				"existing_work_id": existingID,
			})
			return
		case "replace":
			if err := s.store.DeleteWork(existingID); err != nil {
				writeServerError(w, r, err)
				return
			}
			replaced = true
		default: // "prompt" / unset / unknown → let the client decide.
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":                   "conflict",
				"title":                    manifest.Title,
				"author":                   manifest.Author,
				"existing_work_id":         existingID,
				"existing_content_version": existingCV,
				"incoming_content_version": manifest.ContentVersion,
				// content_version is an RFC3339 UTC stamp — lexically sortable.
				"incoming_newer": manifest.ContentVersion > existingCV,
				"message":        "resend with ?on_conflict=replace (newer wins), skip, or new",
			})
			return
		}
	}

	if err := abook.Import(s.store, tmpPath, s.LibraryDir); err != nil {
		writeServerError(w, r, err)
		return
	}
	s.Events.Broadcast(Event{Type: "library_updated"})
	s.EmbedNewWorks() // #159b: embed the imported work without a restart

	newID, _, _, _ := s.store.FindWorkByTitleAuthor(manifest.Title, manifest.Author)
	status := "imported"
	if replaced {
		status = "replaced"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status, "work_id": newID, "title": manifest.Title,
	})
}

// handleSettingsSchema serves the backend-driven settings schema (#202) — the
// single source of truth web + mobile render their settings UIs from, so they
// stop drifting against the flat KV. Static + cacheable; values still come
// from GET /api/settings.
func (s *Server) handleSettingsSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, SettingsSchema())
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetAllSettings()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	// Mask any field whose name implies it's a secret so the value can't
	// be read off the wire by anyone who can reach /api/settings. The
	// saved value still works — handleSaveSettings recognizes the mask
	// and skips updating those fields, so users only need to retype a
	// secret when actually rotating it.
	for k, v := range settings {
		if isSecretSettingKey(k) && v != "" {
			settings[k] = maskSecret(v)
		}
	}
	writeJSON(w, http.StatusOK, settings)
}

// isSecretSettingKey matches any settings name that should never leave
// the server in plaintext. Conservative: any "_api_key", "_secret",
// "_token", "_password", or "_hash" suffix. "_hash" covers
// auth_password_hash so the bcrypt digest never ships to a client via
// GET /api/settings (#197).
func isSecretSettingKey(k string) bool {
	for _, suf := range []string{"_api_key", "_secret", "_token", "_password", "_hash"} {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
}

// maskSecret returns a masked rendering of a secret that exposes
// enough prefix and suffix for the user to identify which key is
// installed (different OpenAI projects share the "sk-proj-" prefix
// but diverge in the random middle, so showing the first ~8 and the
// last 4 makes "sk-proj-IsAbC…Xy9z" easy to recognize without
// revealing the rotatable middle).
//
//   "sk-proj-IsAbCdEf...XyzXy9z" -> "sk-proj-…Xy9z"
//   "sk-ant-IsAb...xyzXy9z"      -> "sk-ant-Is…Xy9z"
//   "shortkey"                   -> "****"
//
// Format chosen so isMaskedSecret can detect the placeholder
// unambiguously (presence of the unicode ellipsis '…' which a real
// key would never contain).
func maskSecret(v string) string {
	if len(v) < 12 {
		return "****"
	}
	prefixLen := 8
	// For non sk-proj/sk-ant style keys (no '-' inside the first 8),
	// 4 chars of prefix is enough.
	if !strings.ContainsAny(v[:prefixLen], "-_") {
		prefixLen = 4
	}
	return v[:prefixLen] + "…" + v[len(v)-4:]
}

// isMaskedSecret returns true for the placeholder shape produced by
// maskSecret. The unicode ellipsis character is the marker; real keys
// are URL-safe and won't contain it.
func isMaskedSecret(v string) bool {
	return strings.Contains(v, "…")
}

// handleListLLMModels returns curated model lists per provider for the
// Settings UI dropdown. Hardcoded — providers don't all expose a public
// list endpoint, and even when they do (OpenAI, OpenRouter) the noise of
// returning every variant outweighs the benefit. Users who want a model
// outside this list can pick "Custom" in the UI.
func (s *Server) handleListLLMModels(w http.ResponseWriter, r *http.Request) {
	type modelInfo struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	out := map[string][]modelInfo{
		"anthropic": {
			{ID: "claude-opus-4-20250514", Label: "Claude Opus 4 — flagship"},
			{ID: "claude-sonnet-4-20250514", Label: "Claude Sonnet 4 — balanced (default)"},
			{ID: "claude-3-5-haiku-20241022", Label: "Claude 3.5 Haiku — fast"},
		},
		"openai": {
			{ID: "gpt-4o", Label: "GPT-4o — flagship"},
			{ID: "gpt-4o-mini", Label: "GPT-4o mini — cheap, very capable"},
			{ID: "gpt-4-turbo", Label: "GPT-4 Turbo"},
			{ID: "o1", Label: "o1 — reasoning"},
			{ID: "o1-mini", Label: "o1 mini — reasoning, cheaper"},
			{ID: "gpt-3.5-turbo", Label: "GPT-3.5 Turbo — legacy"},
		},
		"openrouter": {
			{ID: "anthropic/claude-3.5-sonnet", Label: "Claude 3.5 Sonnet"},
			{ID: "anthropic/claude-3.5-haiku", Label: "Claude 3.5 Haiku"},
			{ID: "openai/gpt-4o", Label: "OpenAI GPT-4o"},
			{ID: "openai/gpt-4o-mini", Label: "OpenAI GPT-4o mini (default)"},
			{ID: "google/gemini-pro-1.5", Label: "Google Gemini Pro 1.5"},
			{ID: "google/gemini-flash-1.5", Label: "Google Gemini Flash 1.5"},
			{ID: "meta-llama/llama-3.1-405b-instruct", Label: "Llama 3.1 405B"},
			{ID: "meta-llama/llama-3.1-70b-instruct", Label: "Llama 3.1 70B"},
			{ID: "mistralai/mistral-large", Label: "Mistral Large"},
		},
		"ollama": {
			{ID: "llama3.2", Label: "Llama 3.2 (default — pull first)"},
			{ID: "llama3.1", Label: "Llama 3.1"},
			{ID: "qwen2.5", Label: "Qwen 2.5"},
			{ID: "mistral", Label: "Mistral 7B"},
			{ID: "gemma2", Label: "Gemma 2"},
		},
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEmbeddingsCoverage reports how much of the chunk table has
// embeddings populated. RAG silently falls back to keyword search for
// any chunk missing an embedding — so a low percentage means most of
// the library's Q&A is degraded even when the LLM is configured.
// Settings surfaces this so the gap is visible.
//
// Shape:
//
//	{"total": 57039, "embedded": 10234, "percent": 17.9, "llm_configured": true}
//
// llm_configured tells the UI whether the "Refresh embeddings" button
// is actionable (no LLM → no embeddings can be computed).
func (s *Server) handleEmbeddingsCoverage(w http.ResponseWriter, r *http.Request) {
	total, embedded, err := s.store.EmbeddingCoverage()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	pct := 0.0
	if total > 0 {
		pct = float64(embedded) / float64(total) * 100
	}
	llmReady := false
	if rag := s.RAG(); rag != nil && rag.Client() != nil {
		llmReady = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":          total,
		"embedded":       embedded,
		"percent":        pct,
		"llm_configured": llmReady,
	})
}

// handleEmbeddingsRefresh kicks off a backfill across every work in
// the library. Each per-work embed short-circuits when all the work's
// chunks already have embeddings, so re-running is cheap. Returns the
// number of works queued; actual progress lands in the System Console
// via applog (embed/rag component).
//
// Pre-existing infrastructure: embedAllWorks runs the same loop on
// nil→client LLM transitions (#159), so a fresh key auto-backfills.
// This endpoint is the manual button for "I want to backfill now"
// without needing to re-enter the key.
func (s *Server) handleEmbeddingsRefresh(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil || rag.Client() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no LLM configured — set a provider/key in Settings",
		})
		return
	}
	works, err := s.store.ListWorks()
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	go s.embedAllWorks()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"works_queued": len(works),
		"status":       "started",
	})
}

// handleLibraryRescan runs library.Rescan synchronously and returns
// its summary. Safety valve for "the watcher missed something" —
// rare but real on NFS/sshfs/fast-create-then-write sequences. Sync
// because the operation is bounded (~1s for an ~800-file library)
// and the UI wants a concrete result to render. Re-runs are safe;
// every step short-circuits when the row/sidecar/chapters already
// exist.
func (s *Server) handleLibraryRescan(w http.ResponseWriter, r *http.Request) {
	if s.LibraryDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "library path not configured"})
		return
	}
	result, err := Rescan(s.store, s.LibraryDir)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	// Nudge listeners so the web/mobile work list reflects newly-created
	// works without a manual refresh — mirrors the broadcast on import.
	if s.Events != nil {
		s.Events.Broadcast(Event{Type: "library_updated"})
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDiskUsage reports the on-disk footprint of the library and
// generated-audio directories plus the filesystem free space available
// to each. The Settings page surfaces this so an impending "no space
// left on device" — the kind that just blocked a sibling MOBI write
// during development — is visible *before* it bites a real ingest.
//
// Shape:
//
//	{
//	  "library":   {"path": "/library",   "used": 12839848, "free": 6442450944},
//	  "generated": {"path": "/generated", "used":  210763,   "free": 6442450944}
//	}
//
// Either entry is omitted when its server-side path isn't configured.
// `free` is the user-available number (Statfs_t.Bavail × Bsize), the
// same one `df -h` shows in its "Avail" column.
func (s *Server) handleDiskUsage(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if s.LibraryDir != "" {
		out["library"] = pathDiskStats(s.LibraryDir)
	}
	if s.GeneratedDir != "" {
		out["generated"] = pathDiskStats(s.GeneratedDir)
	}
	writeJSON(w, http.StatusOK, out)
}

// pathDiskStats walks `path` to total used bytes and queries the
// containing filesystem for available bytes. Errors fall through as
// zero — better to show "0 bytes" than to fail the whole settings
// page over a missing dir on first boot.
func pathDiskStats(path string) map[string]any {
	var used int64
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		used += info.Size()
		return nil
	})
	free := fsFreeBytes(path) // platform-split (diskfree_{unix,windows}.go)
	return map[string]any{
		"path": path,
		"used": used,
		"free": free,
	}
}

// handleTestLLM probes the configured LLM with a single-token completion
// so the Settings UI can confirm a key is valid before the user trusts
// Q&A with it. POST body is fully optional — any missing field falls
// back to the stored setting, so the button works for "test what's
// saved" as well as "test the key I just pasted but haven't saved".
//
// Returns 200 in both success and failure cases (the failure is part
// of the legitimate response, not an HTTP error). Shape:
//
//	{"ok": true,  "latency_ms": 320, "model": "gpt-4o"}
//	{"ok": false, "latency_ms":  80, "error": "openai error 401: ..."}
func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
		BaseURL  string `json:"base_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	settings, _ := s.store.GetAllSettings()
	if body.Provider == "" {
		body.Provider = settings["llm_provider"]
	}
	// Masked secret in the body means "the UI didn't touch the key" —
	// fall through to the stored value just like handleSaveSettings does.
	if body.APIKey == "" || isMaskedSecret(body.APIKey) {
		body.APIKey = settings["llm_api_key"]
	}
	if body.Model == "" {
		body.Model = settings["llm_model"]
	}
	if body.BaseURL == "" {
		body.BaseURL = settings["llm_base_url"]
	}

	if body.Provider == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no provider configured"})
		return
	}
	if body.Provider != "ollama" && body.APIKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "no API key set"})
		return
	}

	client := llm.NewClient(llm.Provider(body.Provider), body.APIKey, body.Model, body.BaseURL)
	start := time.Now()
	_, err := client.Complete(llm.CompletionRequest{
		Messages:  []llm.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 5,
	})
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"error":      err.Error(),
			"latency_ms": latencyMs,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"latency_ms": latencyMs,
		"model":      client.Model(),
	})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	// auth_password is write-only: hash it into auth_password_hash and
	// drop the plaintext so it never lands in the settings KV. An empty
	// value clears the hash, which disables auth (back to an open
	// server). The bare auth_password key is never persisted. (#197)
	if pw, ok := body["auth_password"]; ok {
		delete(body, "auth_password")
		if strings.TrimSpace(pw) == "" {
			if err := s.store.SetSetting("auth_password_hash", ""); err != nil {
				writeServerError(w, r, err)
				return
			}
		} else {
			hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "hash password: " + err.Error()})
				return
			}
			if err := s.store.SetSetting("auth_password_hash", string(hash)); err != nil {
				writeServerError(w, r, err)
				return
			}
		}
	}
	// Reserved keys have dedicated mutation paths and must NEVER be written
	// straight from the settings body. auth_password_hash is derived from
	// auth_password via bcrypt above — letting a client install a chosen
	// digest would, on an open server (auth off, the default), let anyone
	// reachable enable auth with a password only they know and lock out the
	// owner; it's also defense-in-depth against CSRF when auth is on.
	// server_install_id is rotated via POST /api/server-id/rotate. Drop them.
	for _, k := range []string{"auth_password_hash", "server_install_id"} {
		delete(body, k)
	}
	llmTouched := false
	for k, v := range body {
		// If a secret field came back with the mask placeholder, the
		// user didn't touch it on this save — keep the existing value
		// instead of clobbering it with "****Xy9z".
		if isSecretSettingKey(k) && isMaskedSecret(v) {
			continue
		}
		if err := s.store.SetSetting(k, v); err != nil {
			writeServerError(w, r, err)
			return
		}
		if strings.HasPrefix(k, "llm_") {
			llmTouched = true
		}
	}
	// Rebuild the LLM client when any llm_* key changed so the next
	// /api/works/{id}/ask call uses the freshly-saved key/model without
	// a server restart (#160).
	if llmTouched {
		s.ReloadLLM()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getBookByID(w http.ResponseWriter, r *http.Request) (*db.Book, error) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return nil, err
	}

	book, err := s.store.GetBook(id)
	if err != nil {
		writeServerError(w, r, err)
		return nil, err
	}
	if book == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return nil, nil
	}

	return book, nil
}

// accessLogMiddleware prints one line per HTTP request:
//   ACCESS 2026-05-21T20:14:01 ip=10.0.0.4 fwd=1.2.3.4 GET /api/works 200 1234b 12ms
// `ip` is the immediate peer (the nullbore tunnel container when
// served via the relay) and `fwd` is the original client IP from
// X-Forwarded-For (or "-" when absent). Both must be examined to
// catch off-prem traffic — a "GET /api/settings" with fwd != 127.0.0.1
// or the host LAN is exactly the access pattern that would have
// exfiltrated llm_api_key before this commit.
//
// WebSocket pings + static asset GETs are noisy and security-uninteresting
// once the WS upgrade and the page load are themselves logged, so they
// are dropped from the access log to keep the signal high.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if isAccessLogNoise(r) {
			return
		}
		fwd := r.Header.Get("X-Forwarded-For")
		if fwd == "" {
			fwd = "-"
		}
		log.Printf("ACCESS ip=%s fwd=%s %s %s %d %db %s",
			clientIP(r.RemoteAddr), fwd,
			r.Method, r.URL.Path, rw.status, rw.bytes,
			time.Since(start).Truncate(time.Millisecond))
	})
}

// isAccessLogNoise drops repeating low-signal lines so the log can be
// usefully grepped. We keep the WS upgrade itself (it shows the client
// connecting) but skip subsequent ping frames; we keep page loads but
// skip the static asset cascade (CSS, JS, images, favicon).
func isAccessLogNoise(r *http.Request) bool {
	p := r.URL.Path
	if p == "/api/health" || p == "/api/ready" {
		return true // polled in tight loops (containers, desktop shell startup)
	}
	if strings.HasPrefix(p, "/shared/") || strings.HasPrefix(p, "/static/") {
		return true
	}
	switch p {
	case "/favicon.ico", "/manifest.json", "/robots.txt":
		return true
	}
	// Static asset extensions inside any path.
	for _, ext := range []string{".css", ".js", ".png", ".jpg", ".jpeg", ".svg", ".ico", ".woff", ".woff2"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// statusRecorder captures the response status + bytes written so the
// access log can record them. Wraps the underlying ResponseWriter
// transparently; supports Hijack so WebSocket upgrade still works.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(s int) {
	r.status = s
	r.ResponseWriter.WriteHeader(s)
}
func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func clientIP(remoteAddr string) string {
	if i := strings.LastIndex(remoteAddr, ":"); i > 0 {
		return remoteAddr[:i]
	}
	return remoteAddr
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// writeServerError logs the underlying error server-side (so the detail lands
// in the logs / System Console) and returns a GENERIC 500 to the client. This
// keeps internal paths, SQL, and driver text off the wire — relevant on a
// public tunnel where an error body would otherwise be an info-disclosure
// channel. Use for unexpected internal failures (not for client-actionable
// 4xx, where a specific message helps).
func writeServerError(w http.ResponseWriter, r *http.Request, err error) {
	path := ""
	if r != nil {
		path = r.Method + " " + r.URL.Path
	}
	applog.Log(applog.LevelError, "server", "", 0, "request failed", map[string]any{
		"path":  path,
		"error": err.Error(),
	})
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
}
