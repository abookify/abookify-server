package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

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
	RAG        *llm.RAG
	LibraryDir   string
	GeneratedDir string
}

func New(store *db.Store, port string) *Server {
	s := &Server{store: store, Events: NewEventBus()}

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/pair-qr", s.handlePairQR)
	mux.HandleFunc("GET /api/pair-payload", s.handlePairPayload)
	mux.HandleFunc("GET /api/server-info", s.handleServerInfo)
	mux.HandleFunc("GET /api/books", s.handleListBooks)
	mux.HandleFunc("GET /api/books/{id}", s.handleGetBook)
	mux.HandleFunc("GET /api/books/{id}/stream", s.handleStreamBook)
	mux.HandleFunc("GET /api/works", s.handleListWorks)
	mux.HandleFunc("GET /api/works/{id}", s.handleGetWork)
	mux.HandleFunc("GET /api/works/{id}/cover", s.handleWorkCover)
	mux.HandleFunc("GET /api/books/{id}/chapters", s.handleListChapters)
	mux.HandleFunc("GET /api/books/{id}/chapters/{index}", s.handleGetChapter)
	mux.HandleFunc("GET /api/books/{id}/search", s.handleSearchBook)
	mux.HandleFunc("GET /api/works/{id}/search", s.handleSearchWork)
	mux.HandleFunc("GET /api/books/{id}/waveform", s.handleWaveform)
	mux.HandleFunc("POST /api/works/{id}/ask", s.handleAskQuestion)
	mux.HandleFunc("POST /api/works/{id}/generate-audio", s.handleGenerateAudio)
	mux.HandleFunc("POST /api/works/{id}/transcribe", s.handleTranscribe)
	mux.HandleFunc("POST /api/works/{id}/detect-chapters", s.handleDetectChapters)
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
	mux.HandleFunc("GET /api/works/{id}/position", s.handleGetPosition)
	mux.HandleFunc("POST /api/works/{id}/position", s.handleSavePosition)
	mux.HandleFunc("GET /api/tts/preview", s.handleTTSPreview)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings", s.handleSaveSettings)
	mux.HandleFunc("GET /api/works/{id}/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("POST /api/works/{id}/bookmarks", s.handleCreateBookmark)
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
		Handler: corsMiddleware(mux),
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

func (s *Server) handleAskQuestion(w http.ResponseWriter, r *http.Request) {
	if s.RAG == nil {
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
		Question string `json:"question"`
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
	answer, err := library.AskWithCitations(s.store, s.RAG, workID, req.Question)
	if err != nil {
		// Legacy fallback: keyword-only search on the first text file
		legacy, err2 := s.RAG.Ask(work.TextFiles[0].ID, req.Question, work.Title)
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
	var req struct {
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.store.UpdateWork(workID, req.Title, req.Author); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
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
	if s.RAG == nil || s.RAG.Client() == nil {
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

	resp, err := library.Converse(s.store, s.Generator.STTClient(), s.Generator.TTSClient(), s.RAG, workID, tmpPath, voice)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	if s.RAG == nil || s.RAG.Client() == nil {
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
		n, err := library.EmbedChunksForBook(s.store, s.RAG.Client(), tf.ID)
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

	if err := abook.Export(s.store, work, tmpPath); err != nil {
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
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	for k, v := range body {
		if err := s.store.SetSetting(k, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
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
