package server

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pj/abookify/internal/abook"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
	"github.com/pj/abookify/internal/scanner"
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

	// embedding dedupe — guards against re-entry when multiple STT jobs
	// finish back-to-back for the same work, or when ReloadLLM kicks off
	// a backfill while a per-work embed is already running.
	embedMu      sync.Mutex
	embedInFlight map[int64]bool
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
		go s.embedAllWorks()
	}
}

// OnJobUpdate is the Generator's job-update callback. It broadcasts the
// update to WebSocket subscribers and, on STT completion, kicks off a
// background chunk+embed for the work (#159) so a newly transcribed
// audiobook becomes searchable without a manual /api/works/{id}/embed.
func (s *Server) OnJobUpdate(job library.JobStatus) {
	s.Events.Broadcast(Event{Type: "job_update", Data: job})
	if job.Type == "stt" && job.Status == "completed" && job.WorkID > 0 {
		go s.EmbedWorkAsync(job.WorkID)
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

// embedAllWorks runs EmbedWorkAsync for every work in the library.
// Fired on the LLM-enable transition so works added while no LLM was
// configured get backfilled. Each per-work embed is a no-op when its
// chunks already have embeddings.
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

func New(store *db.Store, port string) *Server {
	s := &Server{
		store:         store,
		Events:        NewEventBus(),
		embedInFlight: make(map[int64]bool),
	}

	// Drop any login tokens that expired while the server was down (#197).
	// Per-request validation also purges lazily; this keeps the table tidy.
	_ = store.PurgeExpiredAuthSessions()

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/health", s.handleHealth)
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
	mux.HandleFunc("GET /api/works/{id}/cover", s.handleWorkCover)
	mux.HandleFunc("POST /api/works/{id}/fetch-cover", s.handleFetchCover)
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
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/works/{id}/generate-audio", s.handleGenerateAudio)
	mux.HandleFunc("POST /api/works/{id}/transcribe", s.handleTranscribe)
	mux.HandleFunc("POST /api/works/{id}/detect-chapters", s.handleDetectChapters)
	mux.HandleFunc("POST /api/books/{id}/chapters", s.handleAddChapter)
	mux.HandleFunc("POST /api/works/{id}/align", s.handleForceAlign)
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
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings", s.handleSaveSettings)
	mux.HandleFunc("GET /api/llm/models", s.handleListLLMModels)
	mux.HandleFunc("GET /api/works/{id}/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("POST /api/works/{id}/bookmarks", s.handleCreateBookmark)
	mux.HandleFunc("PUT /api/bookmarks/{id}", s.handleUpdateBookmark)
	mux.HandleFunc("DELETE /api/bookmarks/{id}", s.handleDeleteBookmark)
	mux.HandleFunc("GET /api/works/{id}/export.abook", s.handleExportAbook)
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "abookify",
		"version": "0.1.0",
		"port":    s.http.Addr,
	})
}

func (s *Server) handleListBooks(w http.ResponseWriter, r *http.Request) {
	books, err := s.store.ListBooks()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if works == nil {
		works = []db.Work{}
	}
	writeJSON(w, http.StatusOK, works)
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

func (s *Server) handleListChapters(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimSpace(r.PathValue("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	chapters, err := s.store.ListChapters(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("merged work %d into work %d", req.SourceID, targetID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "merged"})
}

func (s *Server) handleDeleteWork(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.DeleteWork(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("deleted work %d", id)
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	// Trigger a library rescan in the background.
	go func() {
		results, err := scanner.Scan(s.LibraryDir)
		if err != nil {
			log.Printf("upload: rescan failed: %v", err)
			return
		}
		for _, r := range results {
			s.store.UpsertBook(r)
		}
		library.MatchAndCreateWorks(s.store)
		log.Printf("upload: rescan complete, %d files found", len(results))
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	aligned, conf, err := library.ComputeTranscriptEbookAlignment(s.store, workID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chapters_aligned":    aligned,
		"average_confidence":  conf,
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chapters_detected": n})
}

func (s *Server) handleGetSyncData(w http.ResponseWriter, r *http.Request) {
	workID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	audioBookID, _ := strconv.ParseInt(r.PathValue("audioBookId"), 10, 64)
	chapterIdx, _ := strconv.Atoi(r.PathValue("chapterIdx"))

	data, err := s.store.GetSyncData(workID, audioBookID, chapterIdx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Record a 10-second listening event for analytics. The web/mobile
	// clients save position every 10 seconds while playing — each save
	// represents ~10s of actual listening time.
	s.store.RecordPlayback(workID, "listen", 10)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	if err := abook.ExportWithDirs(s.store, work, tmpPath, s.LibraryDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	safeName := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '"' {
			return '-'
		}
		return r
	}, work.Title)

	w.Header().Set("Content-Type", "application/x-abook+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.abook"`, safeName))
	http.ServeFile(w, r, tmpPath)
}

func (s *Server) handleImportAbook(w http.ResponseWriter, r *http.Request) {
	// Limit upload to 500MB
	r.Body = http.MaxBytesReader(w, r.Body, 500<<20)

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file"})
		return
	}
	defer file.Close()

	if !strings.HasSuffix(header.Filename, ".abook") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file must have .abook extension"})
		return
	}

	// Save to temp file
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

	if err := abook.Import(s.store, tmpPath, s.LibraryDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.Events.Broadcast(Event{Type: "library_updated"})
	writeJSON(w, http.StatusOK, map[string]string{"status": "imported"})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetAllSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		} else {
			hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "hash password: " + err.Error()})
				return
			}
			if err := s.store.SetSetting("auth_password_hash", string(hash)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
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
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	if p == "/api/health" {
		return true
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
