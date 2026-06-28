package library

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/stt"
	"github.com/pj/abookify/internal/tts"
)

// JobStatus tracks a running generation/transcription job.
type JobStatus struct {
	ID          string  `json:"id"`
	WorkID      int64   `json:"work_id"`
	Type        string  `json:"type"` // "tts" or "stt"
	Status      string  `json:"status"` // "running", "completed", "failed", "interrupted"
	Progress    float64 `json:"progress"` // 0.0 to 1.0
	CurrentStep string  `json:"current_step"`
	Error       string  `json:"error,omitempty"`
	ETA         string  `json:"eta,omitempty"` // estimated time remaining
	startedAt   time.Time
}

// queuedJob represents a job waiting to run.
type queuedJob struct {
	job  *JobStatus
	run  func()
}

// Generator orchestrates TTS and STT jobs with a single-worker queue.
type Generator struct {
	store        *db.Store
	ttsClient    *tts.Client
	sttClient    *stt.Client
	generatedDir string
	// libraryRoot is where sidecars live next to their audiobook
	// directories — needed by the redo-STT path so it can locate the
	// existing sidecar and write it back atomically.
	libraryRoot string
	onUpdate    func(JobStatus)

	mu      sync.Mutex
	running map[string]bool
	// lastStatus is the last status we logged per job id, so updateJob
	// only emits a structured log on an actual transition (not every
	// progress tick). Guarded by mu.
	lastStatus map[string]string

	queue chan queuedJob
}

// SetLibraryRoot lets the server inject the library root after the
// Generator is constructed. Optional — only needed for the redo-STT
// path; other jobs don't touch sidecar files on disk.
func (g *Generator) SetLibraryRoot(p string) { g.libraryRoot = p }

func NewGenerator(store *db.Store, ttsClient *tts.Client, sttClient *stt.Client, generatedDir string, onUpdate func(JobStatus)) *Generator {
	os.MkdirAll(generatedDir, 0755)

	// Collect jobs that were running/queued before the restart so we can
	// re-queue them after the worker is up.
	jobs, _ := store.ListJobs()
	var resumable []db.Job
	for _, j := range jobs {
		if j.Status == "running" || j.Status == "queued" {
			resumable = append(resumable, j)
			j.Status = "interrupted"
			j.Error = "server restarted — will auto-resume"
			store.UpsertJob(j)
			applog.JobEvent(applog.LevelWarn, j.ID, j.WorkID,
				"job interrupted: server restarted — will auto-resume",
				map[string]any{"type": j.Type})
		}
	}

	g := &Generator{
		store:        store,
		ttsClient:    ttsClient,
		sttClient:    sttClient,
		generatedDir: generatedDir,
		onUpdate:     onUpdate,
		running:      make(map[string]bool),
		lastStatus:   make(map[string]string),
		queue:        make(chan queuedJob, 50),
	}

	// Single worker processes jobs sequentially
	go g.worker()

	// Auto-resume interrupted jobs from the previous run.
	if len(resumable) > 0 {
		go func() {
			time.Sleep(2 * time.Second) // let the rest of init finish
			for _, j := range resumable {
				applog.JobEvent(applog.LevelInfo, j.ID, j.WorkID, "auto-resuming job after restart",
					map[string]any{"type": j.Type})
				switch j.Type {
				case "tts":
					g.GenerateAudioFromText(j.WorkID, 0, "") // will find text book + voice from settings
				case "stt":
					g.TranscribeAudio(j.WorkID)
				}
			}
		}()
	}

	return g
}

func (g *Generator) worker() {
	for qj := range g.queue {
		g.setRunning(qj.job.ID, true)
		qj.job.Status = "running"
		qj.job.CurrentStep = "Starting..."
		g.updateJob(qj.job)

		qj.run()

		g.setRunning(qj.job.ID, false)
	}
}

func (g *Generator) TTSClient() *tts.Client { return g.ttsClient }
func (g *Generator) STTClient() *stt.Client { return g.sttClient }

func (g *Generator) GetJobs() []JobStatus {
	dbJobs, _ := g.store.ListJobs()
	result := make([]JobStatus, 0, len(dbJobs))
	for _, j := range dbJobs {
		result = append(result, JobStatus{
			ID: j.ID, WorkID: j.WorkID, Type: j.Type,
			Status: j.Status, Progress: j.Progress,
			CurrentStep: j.CurrentStep, Error: j.Error,
		})
	}
	return result
}

func (g *Generator) GetJob(id string) *JobStatus {
	j, err := g.store.GetJob(id)
	if err != nil || j == nil {
		return nil
	}
	return &JobStatus{
		ID: j.ID, WorkID: j.WorkID, Type: j.Type,
		Status: j.Status, Progress: j.Progress,
		CurrentStep: j.CurrentStep, Error: j.Error,
	}
}

func (g *Generator) isRunning(jobID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[jobID]
}

func (g *Generator) setRunning(jobID string, running bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if running {
		g.running[jobID] = true
	} else {
		delete(g.running, jobID)
	}
}

func (g *Generator) updateJob(job *JobStatus) {
	g.store.UpsertJob(db.Job{
		ID: job.ID, WorkID: job.WorkID, Type: job.Type,
		Status: job.Status, Progress: job.Progress,
		CurrentStep: job.CurrentStep, Error: job.Error,
	})
	g.logJobTransition(job)
	if g.onUpdate != nil {
		g.onUpdate(*job)
	}
}

// logJobTransition emits a structured log only when a job's status
// actually changes (queued→running→completed/failed/interrupted), so
// progress ticks don't spam the console but every outcome — especially
// *why* a job failed — is recorded for the System Console.
func (g *Generator) logJobTransition(job *JobStatus) {
	g.mu.Lock()
	prev := g.lastStatus[job.ID]
	g.lastStatus[job.ID] = job.Status
	g.mu.Unlock()
	if prev == job.Status {
		return
	}
	fields := map[string]any{"type": job.Type}
	if job.CurrentStep != "" {
		fields["step"] = job.CurrentStep
	}
	switch job.Status {
	case "failed":
		fields["error"] = job.Error
		applog.JobEvent(applog.LevelError, job.ID, job.WorkID, "job failed: "+job.Error, fields)
	case "interrupted":
		applog.JobEvent(applog.LevelWarn, job.ID, job.WorkID, "job interrupted: "+job.Error, fields)
	case "completed":
		applog.JobEvent(applog.LevelInfo, job.ID, job.WorkID, "job completed", fields)
	case "running":
		applog.JobEvent(applog.LevelInfo, job.ID, job.WorkID, "job started", fields)
	case "queued":
		applog.JobEvent(applog.LevelInfo, job.ID, job.WorkID, "job queued", fields)
	}
}

// GenerateAudioFromText creates audio files for a text book using TTS.
// Runs asynchronously. Returns job ID, or empty string if already running.
func (g *Generator) GenerateAudioFromText(workID int64, bookID int64, voice string) (string, bool) {
	jobID := fmt.Sprintf("tts-%d-%d", workID, bookID)

	// Prevent duplicate jobs
	if g.isRunning(jobID) {
		return jobID, false
	}
	// Also check if already queued
	if existing, _ := g.store.GetJob(jobID); existing != nil && (existing.Status == "running" || existing.Status == "queued") {
		return jobID, false
	}

	job := &JobStatus{
		ID:     jobID,
		WorkID: workID,
		Type:   "tts",
		Status: "queued",
	}
	job.CurrentStep = "Waiting in queue..."
	g.updateJob(job)

	g.queue <- queuedJob{
		job: job,
		run: func() { g.runTTS(job, bookID, voice) },
	}

	return jobID, true
}

func (g *Generator) runTTS(job *JobStatus, bookID int64, voice string) {
	job.startedAt = time.Now()

	chapters, err := g.store.ListChapters(bookID)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		g.updateJob(job)
		return
	}

	if len(chapters) == 0 {
		job.Status = "failed"
		job.Error = "no chapters to synthesize"
		g.updateJob(job)
		return
	}

	// Create output directory for this book
	outDir := filepath.Join(g.generatedDir, fmt.Sprintf("tts-book-%d", bookID))
	os.MkdirAll(outDir, 0755)

	if voice == "" || strings.HasPrefix(voice, "en_US") {
		voice = "af_heart"
	}

	for i, chMeta := range chapters {
		job.Progress = float64(i) / float64(len(chapters))
		job.CurrentStep = fmt.Sprintf("Generating chapter %d/%d: %s", i+1, len(chapters), chMeta.Title)
		if i > 0 {
			job.ETA = estimateETA(job.startedAt, i, len(chapters))
		}
		g.updateJob(job)

		// Load full chapter content
		ch, err := g.store.GetChapterContent(bookID, chMeta.Index)
		if err != nil || ch == nil {
			log.Printf("tts: skipping chapter %d: %v", chMeta.Index, err)
			continue
		}

		if len(strings.TrimSpace(ch.Content)) < 10 {
			continue
		}

		// Skip Gutenberg boilerplate chapters
		if isBoilerplateTitle(chMeta.Title) {
			log.Printf("tts: skipping boilerplate chapter %q", chMeta.Title)
			continue
		}

		mp3Path := filepath.Join(outDir, fmt.Sprintf("chapter-%03d.mp3", chMeta.Index))

		// Skip if already generated
		if _, err := os.Stat(mp3Path); err == nil {
			continue
		}

		// Preprocess text for natural speech
		processedText := PreprocessForTTS(chMeta.Title, ch.Content)

		// Split long text into chunks for TTS (~500 words each)
		textChunks := SplitTextForTTS(processedText, 500)
		var allAudio []byte

		for ci, chunk := range textChunks {
			if len(textChunks) > 1 {
				job.CurrentStep = fmt.Sprintf("Generating chapter %d/%d: %s (part %d/%d)",
					i+1, len(chapters), chMeta.Title, ci+1, len(textChunks))
				g.updateJob(job)
			}

			audioData, err := g.ttsClient.Synthesize(chunk, voice)
			if err != nil {
				log.Printf("tts: synthesis failed for chapter %d chunk %d: %v", chMeta.Index, ci, err)
				job.Status = "failed"
				job.Error = fmt.Sprintf("chapter %d: %v", chMeta.Index, err)
				g.updateJob(job)
				return
			}
			allAudio = append(allAudio, audioData...)
		}

		if err := os.WriteFile(mp3Path, allAudio, 0644); err != nil {
			job.Status = "failed"
			job.Error = err.Error()
			g.updateJob(job)
			return
		}

		// Register immediately with the source chapter title
		info, _ := os.Stat(mp3Path)
		g.store.UpsertBook(db.Book{
			WorkID:    job.WorkID,
			Path:      mp3Path,
			Filename:  filepath.Base(mp3Path),
			Format:    "mp3",
			MediaType: "audio",
			SizeBytes: info.Size(),
			Title:     chMeta.Title,
			Author:    "Generated by Kokoro TTS",
			Album:     voice,
			Origin:    "tts_kokoro",
		})

		// Create chapter link and run alignment
		allBooks, _ := g.store.ListBooks()
		for _, b := range allBooks {
			if b.Path == mp3Path {
				g.store.InsertChapterLink(job.WorkID, db.ChapterLink{
					AudioBookID: b.ID,
					AudioIndex:  i,
					TextBookID:  bookID,
					TextIndex:   chMeta.Index,
					Confidence:  1.0,
				})

				// Run Whisper alignment to get word-level timestamps
				if g.sttClient != nil {
					job.CurrentStep = fmt.Sprintf("Aligning chapter %d/%d: %s", i+1, len(chapters), chMeta.Title)
					g.updateJob(job)
					// Pass original chapter text for alignment to original words
				origText := ""
				if origCh, err := g.store.GetChapterContent(bookID, chMeta.Index); err == nil && origCh != nil {
					origText = origCh.Content
				}
				if err := AlignChapter(g.store, g.sttClient, job.WorkID, b.ID, i, mp3Path, origText); err != nil {
						log.Printf("tts: alignment failed for chapter %d (non-fatal): %v", chMeta.Index, err)
					}
				}
				break
			}
		}

		log.Printf("tts: generated chapter %d/%d for book %d (%s)", i+1, len(chapters), bookID, chMeta.Title)
	}

	job.Progress = 1.0
	job.Status = "completed"
	job.CurrentStep = fmt.Sprintf("Generated %d chapters", len(chapters))
	g.updateJob(job)

	log.Printf("tts: completed generation for book %d (%d chapters)", bookID, len(chapters))
}

// TranscribeAudio creates text transcripts from audio files using STT.
// Runs asynchronously. Returns job ID and whether it was newly started.
func (g *Generator) TranscribeAudio(workID int64) (string, bool) {
	jobID := fmt.Sprintf("stt-%d", workID)

	if g.isRunning(jobID) {
		return jobID, false
	}
	if existing, _ := g.store.GetJob(jobID); existing != nil && (existing.Status == "running" || existing.Status == "queued") {
		return jobID, false
	}

	job := &JobStatus{
		ID:     jobID,
		WorkID: workID,
		Type:   "stt",
		Status: "queued",
	}
	job.CurrentStep = "Waiting in queue..."
	g.updateJob(job)

	g.queue <- queuedJob{
		job: job,
		run: func() { g.runSTT(job, workID) },
	}

	return jobID, true
}

func (g *Generator) runSTT(job *JobStatus, workID int64) {
	job.startedAt = time.Now()
	work, err := g.store.GetWork(workID)
	if err != nil || work == nil {
		job.Status = "failed"
		job.Error = "work not found"
		g.updateJob(job)
		return
	}

	audioFiles := work.AudioFiles
	if len(audioFiles) == 0 {
		job.Status = "failed"
		job.Error = "no audio files to transcribe"
		g.updateJob(job)
		return
	}

	// We'll create a synthetic "text book" entry to hold the transcripts
	transcriptBookID := int64(0)

	for i, af := range audioFiles {
		job.Progress = float64(i) / float64(len(audioFiles))
		title := af.Title
		if title == "" {
			title = af.Filename
		}
		job.CurrentStep = fmt.Sprintf("Transcribing %d/%d: %s", i+1, len(audioFiles), title)
		if i > 0 {
			job.ETA = estimateETA(job.startedAt, i, len(audioFiles))
		}
		g.updateJob(job)

		result, err := transcribeChunked(g.sttClient, af.Path, func(segIdx, totalSegs int) {
			segProgress := float64(segIdx) / float64(totalSegs)
			fileProgress := (float64(i) + segProgress) / float64(len(audioFiles))
			job.Progress = fileProgress
			job.CurrentStep = fmt.Sprintf("Transcribing %d/%d: %s (segment %d/%d)",
				i+1, len(audioFiles), title, segIdx+1, totalSegs)
			if segIdx > 0 {
				job.ETA = estimateETA(job.startedAt, segIdx, totalSegs)
			}
			g.updateJob(job)
		})
		if err != nil {
			log.Printf("stt: transcription failed for %s: %v", af.Filename, err)
			job.Status = "failed"
			job.Error = fmt.Sprintf("transcription failed for %s: %v", af.Filename, err)
			g.updateJob(job)
			return
		}

		// Create the transcript book on first successful transcription
		if transcriptBookID == 0 {
			transcriptBook := db.Book{
				WorkID:     workID,
				Path:       fmt.Sprintf("generated://transcript/work-%d", workID),
				Filename:   fmt.Sprintf("%s (Transcript)", work.Title),
				Format:     "transcript",
				MediaType:  "text",
				Title:      work.Title + " (Transcript)",
				Author:     work.Author,
				Origin:     "whisper_transcript",
				Visibility: "visible",
			}
			g.store.UpsertBook(transcriptBook)

			// Find the ID of the book we just created
			books, _ := g.store.ListBooks()
			for _, b := range books {
				if b.Path == transcriptBook.Path {
					transcriptBookID = b.ID
					break
				}
			}
		}

		if transcriptBookID > 0 {
			ch := db.Chapter{
				BookID:    transcriptBookID,
				Index:     i,
				Title:     title,
				Content:   result.Text,
				WordCount: len(strings.Fields(result.Text)),
			}
			g.store.InsertChapter(ch)

			// Create chapter link: audio file ↔ transcript chapter
			g.store.InsertChapterLink(workID, db.ChapterLink{
				AudioBookID: af.ID,
				AudioIndex:  i,
				TextBookID:  transcriptBookID,
				TextIndex:   i,
				Confidence:  1.0,
			})
		}

		// Store word-level timestamps as sync data for karaoke playback.
		// These come directly from Whisper (offset-corrected by the chunker).
		var syncTimestamps []db.SyncTimestamp
		for _, seg := range result.Segments {
			for _, w := range seg.Words {
				syncTimestamps = append(syncTimestamps, db.SyncTimestamp{
					Start: w.Start,
					End:   w.End,
					Word:  w.Word,
				})
			}
		}
		if len(syncTimestamps) > 0 {
			data, err := json.Marshal(syncTimestamps)
			if err == nil {
				g.store.SaveSyncData(workID, af.ID, i, string(data))
				log.Printf("stt: stored %d word timestamps for %s", len(syncTimestamps), af.Filename)
			}
		}

		// Chapter layout — only meaningful for single-file audiobooks.
		// Multi-file books already have one book-per-chapter, so pattern-matching
		// the transcript inside each file would be noise.
		if len(audioFiles) == 1 && len(syncTimestamps) > 0 {
			duration := result.Duration
			if duration <= 0 && af.Duration > 0 {
				duration = af.Duration
			}
			// Source of truth, in order of authority: embedded markers >
			// narrator-pattern detection. Either way we end up with a
			// `chapterRanges` list used to split the transcript + relink.
			chapterRanges := embeddedChaptersAsDetected(g.store, af.ID)
			if len(chapterRanges) == 0 {
				chapterRanges = DetectChapters(syncTimestamps, duration)
				if len(chapterRanges) > 0 {
					writeDetectedChapters(g.store, af.ID, chapterRanges)
				}
			}
			if len(chapterRanges) > 0 {
				if transcriptBookID != 0 {
					if _, err := SplitTranscriptByChapters(g.store, transcriptBookID, syncTimestamps, chapterRanges); err != nil {
						log.Printf("split-transcript: %v", err)
					}
				}
				if fresh, _ := g.store.GetWork(workID); fresh != nil {
					if err := LinkChapters(g.store, fresh); err != nil {
						log.Printf("link-chapters after STT: %v", err)
					}
				}
			}
		}

		log.Printf("stt: transcribed %s (%.1fs, %d words)", af.Filename, result.Duration, len(strings.Fields(result.Text)))
	}

	// For multi-file books where filenames don't already encode chapters
	// (e.g. "01.mp3" section splits), try cross-file chapter detection. We
	// infer "no filename hint" from the absence of chapter_links with
	// confidence >= 0.8 (filename-match confidence). If links are strong,
	// detection is skipped to avoid duplicating the filename structure.
	if len(audioFiles) > 1 {
		if fresh, _ := g.store.GetWork(workID); fresh != nil {
			strong := 0
			for _, link := range fresh.ChapterLinks {
				if link.Confidence >= 0.8 {
					strong++
				}
			}
			if strong < len(audioFiles)/2 {
				if n, err := DetectChaptersMultiFile(g.store, workID); err != nil {
					log.Printf("detect-chapters-multifile: %v", err)
				} else if n > 0 {
					log.Printf("detect-chapters-multifile: wrote %d chapters across files", n)
					// Re-link so newly detected chapters can match ebook chapters.
					if relinked, _ := g.store.GetWork(workID); relinked != nil {
						LinkChapters(g.store, relinked)
					}
				}
			}
		}
	}

	job.Progress = 1.0
	job.Status = "completed"
	job.CurrentStep = fmt.Sprintf("Transcribed %d audio files", len(audioFiles))
	g.updateJob(job)

	log.Printf("stt: completed transcription for work %d (%d files)", workID, len(audioFiles))
}

// RegenerateChapter queues a single chapter for audio regeneration.
func (g *Generator) RegenerateChapter(workID, bookID int64, ch *db.Chapter, voice string) (string, bool) {
	jobID := fmt.Sprintf("regen-%d-%d-%d", workID, bookID, ch.Index)

	if g.isRunning(jobID) {
		return jobID, false
	}
	if existing, _ := g.store.GetJob(jobID); existing != nil && existing.Status == "running" {
		return jobID, false
	}

	chCopy := *ch // copy since we're passing to goroutine
	job := &JobStatus{
		ID:     jobID,
		WorkID: workID,
		Type:   "tts",
		Status: "queued",
	}
	job.CurrentStep = fmt.Sprintf("Queued: regenerate %s", ch.Title)
	g.updateJob(job)

	g.queue <- queuedJob{
		job: job,
		run: func() { g.runRegenerateChapter(job, workID, bookID, &chCopy, voice) },
	}

	return jobID, true
}

func (g *Generator) runRegenerateChapter(job *JobStatus, workID, bookID int64, ch *db.Chapter, voice string) {
	job.startedAt = time.Now()
	log.Printf("regenerate: starting chapter %d (%s) for book %d, voice=%s, content=%d chars",
		ch.Index, ch.Title, bookID, voice, len(ch.Content))

	if g.ttsClient == nil {
		log.Printf("regenerate: ERROR - ttsClient is nil")
		return
	}

	outDir := filepath.Join(g.generatedDir, fmt.Sprintf("tts-book-%d", bookID))
	os.MkdirAll(outDir, 0755)

	mp3Path := filepath.Join(outDir, fmt.Sprintf("chapter-%03d.mp3", ch.Index))
	log.Printf("regenerate: output path: %s", mp3Path)

	// Remove old file
	os.Remove(mp3Path)

	// Preprocess
	processed := PreprocessForTTS(ch.Title, ch.Content)
	log.Printf("regenerate: preprocessed to %d chars", len(processed))
	chunks := SplitTextForTTS(processed, 500)

	job.CurrentStep = fmt.Sprintf("Generating: %s", ch.Title)
	g.updateJob(job)

	var allAudio []byte
	for ci, chunk := range chunks {
		job.Progress = float64(ci) / float64(len(chunks))
		if len(chunks) > 1 {
			job.CurrentStep = fmt.Sprintf("Generating: %s (part %d/%d)", ch.Title, ci+1, len(chunks))
		}
		if ci > 0 {
			job.ETA = estimateETA(job.startedAt, ci, len(chunks))
		}
		g.updateJob(job)

		data, err := g.ttsClient.Synthesize(chunk, voice)
		if err != nil {
			log.Printf("regenerate: failed chapter %d: %v", ch.Index, err)
			job.Status = "failed"
			job.Error = fmt.Sprintf("%s: %v", ch.Title, err)
			g.updateJob(job)
			return
		}
		allAudio = append(allAudio, data...)
	}

	if err := os.WriteFile(mp3Path, allAudio, 0644); err != nil {
		log.Printf("regenerate: write failed: %v", err)
		job.Status = "failed"
		job.Error = err.Error()
		g.updateJob(job)
		return
	}

	// Register in library with source chapter title
	info, _ := os.Stat(mp3Path)
	g.store.UpsertBook(db.Book{
		WorkID:    workID,
		Path:      mp3Path,
		Filename:  filepath.Base(mp3Path),
		Format:    "mp3",
		MediaType: "audio",
		SizeBytes: info.Size(),
		Title:     ch.Title,
		Author:    "Generated by Kokoro TTS",
		Album:     voice,
		Origin:    "tts_kokoro",
	})

	// Create/update chapter link
	allBooks, _ := g.store.ListBooks()
	for _, b := range allBooks {
		if b.Path == mp3Path {
			// Find the audio index (position in the work's audio file list)
			work, _ := g.store.GetWork(workID)
			audioIdx := 0
			if work != nil {
				for ai, af := range work.AudioFiles {
					if af.ID == b.ID {
						audioIdx = ai
						break
					}
				}
			}
			g.store.InsertChapterLink(workID, db.ChapterLink{
				AudioBookID: b.ID,
				AudioIndex:  audioIdx,
				TextBookID:  bookID,
				TextIndex:   ch.Index,
				Confidence:  1.0,
			})

			// Run Whisper alignment
			if g.sttClient != nil {
				job.CurrentStep = fmt.Sprintf("Aligning: %s", ch.Title)
				g.updateJob(job)
				if err := AlignChapter(g.store, g.sttClient, workID, b.ID, audioIdx, mp3Path, ch.Content); err != nil {
					log.Printf("regenerate: alignment failed (non-fatal): %v", err)
				}
			}
			break
		}
	}

	// Incremental re-alignment: only this chapter gets re-aligned, not the
	// whole book. Runs silently — failure doesn't affect the regeneration.
	if err := RealignChapter(g.store, workID, ch.Index); err != nil {
		log.Printf("realign-chapter: %v", err)
	}

	job.Progress = 1.0
	job.Status = "completed"
	job.CurrentStep = fmt.Sprintf("Regenerated: %s", ch.Title)
	g.updateJob(job)

	log.Printf("regenerate: completed chapter %d (%s) for book %d", ch.Index, ch.Title, bookID)
}

// splitTextForTTS / splitSentences moved to tts_chunk.go as the exported
// SplitTextForTTS so the server and tts-cli share one implementation.

func estimateETA(startedAt time.Time, completed, total int) string {
	if completed == 0 || startedAt.IsZero() {
		return "estimating..."
	}
	elapsed := time.Since(startedAt)
	if elapsed < 2*time.Second {
		return "estimating..."
	}
	perItem := elapsed / time.Duration(completed)
	remaining := perItem * time.Duration(total-completed)

	if remaining < time.Minute {
		return fmt.Sprintf("~%ds remaining", int(remaining.Seconds()))
	} else if remaining < time.Hour {
		return fmt.Sprintf("~%dm remaining", int(remaining.Minutes()))
	}
	return fmt.Sprintf("~%dh %dm remaining", int(remaining.Hours()), int(remaining.Minutes())%60)
}

// RetryTranscriptionForFiles enqueues a redo-STT job: re-transcribes
// the named files, merges into the sidecar, then triggers reimport.
// Used by the gap-detection modal in the UI.
//
// Returns (jobID, started). When started==false the work already has
// an STT job in flight; the UI should refresh its job-progress view
// instead of erroring.
func (g *Generator) RetryTranscriptionForFiles(workID int64, filenames []string) (string, bool) {
	jobID := fmt.Sprintf("stt-redo-%d", workID)
	if g.isRunning(jobID) {
		return jobID, false
	}
	if existing, _ := g.store.GetJob(jobID); existing != nil && (existing.Status == "running" || existing.Status == "queued") {
		return jobID, false
	}
	if g.libraryRoot == "" {
		return "", false
	}
	job := &JobStatus{
		ID:          jobID,
		WorkID:      workID,
		Type:        "stt-redo",
		Status:      "queued",
		CurrentStep: fmt.Sprintf("Waiting to retry %d file(s)...", len(filenames)),
	}
	g.updateJob(job)
	g.queue <- queuedJob{
		job: job,
		run: func() { g.runRedoSTT(job, workID, filenames) },
	}
	return jobID, true
}

func (g *Generator) runRedoSTT(job *JobStatus, workID int64, filenames []string) {
	job.startedAt = time.Now()
	progress := func(fileIdx, fileCount int, fileName string, segIdx, totalSegs int) {
		segFraction := float64(0)
		if totalSegs > 0 {
			segFraction = float64(segIdx) / float64(totalSegs)
		}
		job.Progress = (float64(fileIdx) + segFraction) / float64(fileCount)
		job.CurrentStep = fmt.Sprintf("Re-transcribing %d/%d: %s (segment %d/%d)",
			fileIdx+1, fileCount, fileName, segIdx+1, totalSegs)
		if segIdx > 0 || fileIdx > 0 {
			step := fileIdx*100 + segIdx
			total := fileCount * 100
			job.ETA = estimateETA(job.startedAt, step, total)
		}
		g.updateJob(job)
	}
	n, err := redoTranscriptionForFiles(g.store, g.sttClient, g.libraryRoot, workID, filenames, progress)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		g.updateJob(job)
		return
	}
	job.Status = "completed"
	job.Progress = 1.0
	job.CurrentStep = fmt.Sprintf("Re-transcribed %d file(s); transcript updated", n)
	g.updateJob(job)
}

