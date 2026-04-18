package library

import (
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

	// Watch root and all subdirectories
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
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
