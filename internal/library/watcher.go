package library

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/pj/abookify/internal/db"
)

var supportedExts = map[string]string{
	".epub": "epub", ".pdf": "pdf", ".mp3": "mp3",
	".m4b": "m4b", ".m4a": "m4a", ".flac": "flac", ".aac": "aac",
}

var audioExts = map[string]bool{
	".mp3": true, ".m4b": true, ".m4a": true, ".flac": true, ".aac": true,
}

// Watcher monitors the library directory for file changes.
type Watcher struct {
	store    *db.Store
	root     string
	watcher  *fsnotify.Watcher
	onChange func() // callback when library changes

	// Debounce: collect events and process in batch
	mu       sync.Mutex
	pending  map[string]bool
	timer    *time.Timer
}

func NewWatcher(store *db.Store, root string, onChange func()) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		store:    store,
		root:     root,
		watcher:  fsw,
		onChange: onChange,
		pending:  make(map[string]bool),
	}

	// Watch root and all subdirectories EXCEPT the ingest queue's working
	// directories. Files there are managed by IngestQueue; the library
	// watcher only sees them after they land in audiobooks/ or ebooks/.
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
			if top == "incoming" || top == "processing" || top == "failed" {
				return filepath.SkipDir
			}
			return fsw.Add(path)
		}
		return nil
	})
	if err != nil {
		fsw.Close()
		return nil, err
	}

	return w, nil
}

func (w *Watcher) Start() {
	go w.loop()
	log.Printf("file watcher started on %s", w.root)
}

func (w *Watcher) Close() error {
	return w.watcher.Close()
}

func (w *Watcher) loop() {
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) {
				w.queuePath(event.Name)
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

func (w *Watcher) queuePath(path string) {
	// Sidecars are .stt.json files — landed here by remote-stt or syncthing.
	// They aren't books themselves; they describe an existing audio book.
	// Queue them in the same debounce buffer so processPending can dispatch.
	//
	// .stt.json.redo files are user-dropped reprocess requests. Treated
	// the same here; processPending recognizes the suffix and runs
	// re-import instead of regular import.
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".stt.json") || strings.HasSuffix(lower, ".stt.json.redo") {
		w.mu.Lock()
		w.pending[path] = true
		if w.timer != nil {
			w.timer.Stop()
		}
		w.timer = time.AfterFunc(2*time.Second, w.processPending)
		w.mu.Unlock()
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := supportedExts[ext]; !ok {
		// Also watch for new directories
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			w.watcher.Add(path)
		}
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending[path] = true

	// Debounce: wait 2 seconds of quiet before processing
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(2*time.Second, w.processPending)
}

func (w *Watcher) processPending() {
	w.mu.Lock()
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	w.pending = make(map[string]bool)
	w.mu.Unlock()

	if len(paths) == 0 {
		return
	}

	log.Printf("watcher: processing %d file changes", len(paths))

	changed := false
	for _, path := range paths {
		lower := strings.ToLower(path)

		// Redo files: user/script-dropped reprocess request. Strip the
		// .redo suffix to find the actual sidecar, force-clear cached
		// data, re-import, then delete the marker file.
		if strings.HasSuffix(lower, ".stt.json.redo") {
			if w.reimportFromRedo(path) {
				changed = true
			}
			continue
		}

		// Sidecar landed: import it for the matching audio book. Idempotent —
		// sidecar_import skips works that already have sync_data, so a
		// repeated rsync write doesn't redo the work.
		if strings.HasSuffix(lower, ".stt.json") {
			if w.importSidecar(path) {
				changed = true
			}
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			// File was removed — could handle deletion here later
			continue
		}
		if info.IsDir() {
			continue
		}

		ext := strings.ToLower(filepath.Ext(path))
		format, ok := supportedExts[ext]
		if !ok {
			continue
		}

		mediaType := "text"
		if audioExts[ext] {
			mediaType = "audio"
		}

		book := db.Book{
			Path:      path,
			Filename:  filepath.Base(path),
			Format:    format,
			MediaType: mediaType,
			SizeBytes: info.Size(),
			Title:     titleFromPath(path),
		}

		// Extract metadata
		meta, err := ExtractMetadata(path)
		if err == nil {
			if meta.Title != "" {
				book.Title = meta.Title
			}
			if meta.Author != "" {
				book.Author = meta.Author
			}
			book.Album = meta.Album
		}

		if err := w.store.UpsertBook(book); err != nil {
			log.Printf("watcher: failed to store %s: %v", path, err)
			continue
		}
		changed = true
		log.Printf("watcher: ingested %s", filepath.Base(path))

		// Extract chapters for EPUBs
		if format == "epub" {
			books, _ := w.store.ListBooks()
			for _, b := range books {
				if b.Path == path {
					count, _ := w.store.ChapterCount(b.ID)
					if count == 0 {
						chapters, err := ExtractEPUBChapters(path, b.ID)
						if err == nil {
							for _, ch := range chapters {
								w.store.InsertChapter(ch)
							}
							log.Printf("watcher: extracted %d chapters from %s", len(chapters), filepath.Base(path))
						}
					}
					break
				}
			}
		}
	}

	if changed {
		// Re-run matching for unassigned books
		if err := MatchAndCreateWorks(w.store); err != nil {
			log.Printf("watcher: matching failed: %v", err)
		}
		if w.onChange != nil {
			w.onChange()
		}
	}
}

func titleFromPath(path string) string {
	name := filepath.Base(path)
	title := strings.TrimSuffix(name, filepath.Ext(name))
	title = strings.ReplaceAll(title, "_", " ")
	title = strings.ReplaceAll(title, "-", " ")
	return title
}

// importSidecar handles a .stt.json file landing while the server is
// running (e.g. rsynced by remote-stt or syncthing). Looks up the audio
// book this sidecar belongs to and runs the import pipeline. Returns
// true if anything was imported (signals onChange to broadcast).
//
// Idempotent: importOneSidecar already short-circuits when sync_data
// exists for the work, so a re-fired event from a partial-write rsync
// doesn't redo the work.
func (w *Watcher) importSidecar(sidecarPath string) bool {
	// Map host path → /library prefix the way the rest of the code expects.
	// Sidecars sit next to the audio they describe; we walk works looking
	// for one whose audio book's findSidecar resolves to this path.
	works, err := w.store.ListWorks()
	if err != nil {
		log.Printf("watcher: list works for sidecar %s: %v", filepath.Base(sidecarPath), err)
		return false
	}

	// Resolve to the absolute host path so string-equality vs findSidecar's
	// returned path is robust against relative-path drift in the watcher
	// stream (fsnotify gives the path as registered, which can be relative).
	absSidecar, err := filepath.Abs(sidecarPath)
	if err != nil {
		absSidecar = sidecarPath
	}

	for _, wk := range works {
		if !wk.HasAudio || len(wk.AudioFiles) == 0 {
			continue
		}
		af := wk.AudioFiles[0]

		// Skip works that already have sync_data — importOneSidecar would
		// no-op anyway, but checking here avoids the file read.
		existing, _ := w.store.GetSyncData(wk.ID, af.ID, 0)
		if existing != "" && existing != "[]" {
			continue
		}

		candidate := findSidecar(af.Path, w.root)
		if candidate == "" {
			continue
		}
		absCandidate, _ := filepath.Abs(candidate)
		if absCandidate != absSidecar {
			continue
		}

		log.Printf("watcher: importing sidecar for work %d (%s)", wk.ID, wk.Title)
		if err := importOneSidecar(w.store, wk.ID, af.ID, sidecarPath); err != nil {
			log.Printf("watcher: sidecar import for %s failed: %v", wk.Title, err)
			return false
		}
		// Re-link audio↔text chapters now that we have new chapter rows.
		if fresh, ferr := w.store.GetWork(wk.ID); ferr == nil && fresh != nil {
			if err := LinkChapters(w.store, fresh); err != nil {
				log.Printf("watcher: link-chapters after sidecar import: %v", err)
			}
		}
		return true
	}

	log.Printf("watcher: sidecar %s has no matching audio work yet (audio not imported?)", filepath.Base(sidecarPath))
	return false
}

// reimportFromRedo handles a .stt.json.redo file dropped next to an
// existing sidecar. Looks up the matching work and dispatches to the
// shared ReimportWork helper, then removes the marker file so it
// doesn't re-fire.
func (w *Watcher) reimportFromRedo(redoPath string) bool {
	// fsnotify fires a Remove event when we delete the redo file at the
	// end of a successful run. That event re-queues this path through
	// the same code path, which would re-trigger the import. Bail if
	// the file is no longer present — the work has already been done.
	if _, err := os.Stat(redoPath); err != nil {
		return false
	}
	sidecarPath := strings.TrimSuffix(redoPath, ".redo")

	works, err := w.store.ListWorks()
	if err != nil {
		log.Printf("watcher: list works for redo %s: %v", filepath.Base(redoPath), err)
		return false
	}
	absSidecar, err := filepath.Abs(sidecarPath)
	if err != nil {
		absSidecar = sidecarPath
	}

	for _, wk := range works {
		if !wk.HasAudio || len(wk.AudioFiles) == 0 {
			continue
		}
		af := wk.AudioFiles[0]
		candidate := findSidecar(af.Path, w.root)
		if candidate == "" {
			continue
		}
		absCandidate, _ := filepath.Abs(candidate)
		if absCandidate != absSidecar {
			continue
		}

		log.Printf("watcher: REDO request for work %d (%s)", wk.ID, wk.Title)
		if err := ReimportWork(w.store, wk.ID, w.root); err != nil {
			log.Printf("watcher: redo import for %s failed: %v", wk.Title, err)
			return false
		}
		// Marker file did its job — remove so a stale watch event doesn't
		// trigger a second redo on next debounce tick.
		if err := os.Remove(redoPath); err != nil {
			log.Printf("watcher: failed to remove redo marker %s: %v", filepath.Base(redoPath), err)
		}
		return true
	}

	log.Printf("watcher: redo %s has no matching work — leaving marker for future scan", filepath.Base(redoPath))
	return false
}

// ReimportWork runs the post-processing pipeline against a work's
// existing sidecar (no re-transcription). Used by the watcher's
// .stt.json.redo handler and the HTTP reprocess endpoint — both share
// the same body so behavior matches across triggers.
//
// Steps:
//   1. Find the sidecar next to the work's first audio book
//   2. Run importOneSidecar — overwrites sync_data + chapter rows in
//      place, rebuilds the transcript book, repopulates paragraphs
//   3. Clear stale RAG chunks (gated on count>0 inside ChunkBook, so
//      they wouldn't refresh otherwise) and rebuild for text books
//   4. Re-link audio↔text chapters against the fresh chapter rows
//
// Caller responsibility: broadcast WS events / return HTTP status.
func ReimportWork(store *db.Store, workID int64, libraryRoot string) error {
	wk, err := store.GetWork(workID)
	if err != nil {
		return fmt.Errorf("get work: %w", err)
	}
	if wk == nil {
		return fmt.Errorf("work %d not found", workID)
	}
	if !wk.HasAudio || len(wk.AudioFiles) == 0 {
		return fmt.Errorf("work %d has no audio book to reimport", workID)
	}
	af := wk.AudioFiles[0]
	sidecarPath := findSidecar(af.Path, libraryRoot)
	if sidecarPath == "" {
		return fmt.Errorf("no sidecar found for work %d (%s)", workID, wk.Title)
	}

	if err := importOneSidecar(store, wk.ID, af.ID, sidecarPath); err != nil {
		return fmt.Errorf("import sidecar: %w", err)
	}

	// Refresh RAG chunks: ChunkBook is idempotent (skips if count>0), so
	// when chapter boundaries shift the stale chunks would survive
	// otherwise. Clear and rebuild so vector search reflects new splits.
	fresh, ferr := store.GetWork(workID)
	if ferr == nil && fresh != nil {
		for _, b := range fresh.AudioFiles {
			store.DeleteChunksByBook(b.ID)
		}
		for _, b := range fresh.TextFiles {
			store.DeleteChunksByBook(b.ID)
			if b.Format == "epub" || b.Format == "transcript" {
				ChunkBook(store, b.ID)
			}
		}
		if err := LinkChapters(store, fresh); err != nil {
			log.Printf("reimport: link-chapters after import: %v", err)
		}
	}
	return nil
}
