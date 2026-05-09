package library

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// IngestQueue is a file-based drop-zone for new audiobooks/ebooks. Users put
// files (or folders) into <library>/incoming/, and this watcher copies them
// into the canonical library directories (audiobooks/ for audio, ebooks/ for
// text). On unrecoverable failure (after maxRetries), the source is moved to
// failed/ with an error log.
//
// Why file-based vs an in-memory queue?
//   - Survives restarts (state lives on disk).
//   - Visible to the user (just look at the folders).
//   - Works across processes/containers if the volume is shared.
//
// The queue does NOT do STT/LLM work itself — its job is only to land the
// file in the right canonical location. The existing library watcher then
// picks it up and runs the normal scan/import pipeline.
type IngestQueue struct {
	libraryRoot string
	watcher     *fsnotify.Watcher
	stopCh      chan struct{}

	// Stable-file detection: a freshly-dropped file is still being written.
	// We wait until size stops changing for stableQuietPeriod before acting.
	mu      sync.Mutex
	pending map[string]time.Time // path → last-seen-time
	timer   *time.Timer

	// onChange is called whenever a queue state transition happens (file
	// arrives in incoming, moves to processing, completes, fails). Used
	// to broadcast a websocket event so the UI re-fetches status.
	onChange func()
}

// QueueStatus is the snapshot returned by /api/queue/status.
type QueueStatus struct {
	Incoming   []QueueEntry `json:"incoming"`
	Processing []QueueEntry `json:"processing"`
	Failed     []QueueEntry `json:"failed"`
}

type QueueEntry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	IsDir     bool   `json:"is_dir,omitempty"`
	Retries   int    `json:"retries,omitempty"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// SetOnChange sets the callback fired on queue state transitions. Safe to
// call before Start.
func (q *IngestQueue) SetOnChange(fn func()) {
	q.onChange = fn
}

// Status returns the current state of the queue: what's waiting, what's
// being processed, what failed.
func (q *IngestQueue) Status() QueueStatus {
	return QueueStatus{
		Incoming:   q.listDir("incoming", false),
		Processing: q.listDir("processing", true),
		Failed:     q.listDir("failed", true),
	}
}

func (q *IngestQueue) listDir(sub string, withMeta bool) []QueueEntry {
	dir := filepath.Join(q.libraryRoot, sub)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]QueueEntry, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		ent := QueueEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
		}
		if !e.IsDir() {
			ent.SizeBytes = info.Size()
		}
		if withMeta && e.IsDir() {
			if data, err := os.ReadFile(filepath.Join(full, ".queue.json")); err == nil {
				var m queueMeta
				if json.Unmarshal(data, &m) == nil {
					ent.Retries = m.Retries
					ent.LastError = m.LastError
					if !m.CreatedAt.IsZero() {
						ent.CreatedAt = m.CreatedAt.Format(time.RFC3339)
					}
				}
			}
		}
		out = append(out, ent)
	}
	return out
}

const (
	maxRetries        = 3
	stableQuietPeriod = 3 * time.Second
	pollInterval      = 5 * time.Second
)

type queueMeta struct {
	JobID     string    `json:"job_id"`
	Original  string    `json:"original_name"`
	Retries   int       `json:"retries"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// NewIngestQueue creates the queue subdirectories under libraryRoot if they
// don't exist and returns a queue ready to Start.
//
// incoming/ is created world-writable (0o777) because the server typically
// runs as a different user (root in the docker container) than the host user
// dropping files in. Without this, host-side `cp file incoming/` fails with
// permission denied. processing/ and failed/ stay 0o755 since only the
// server writes there.
func NewIngestQueue(libraryRoot string) (*IngestQueue, error) {
	for _, sub := range []string{"audiobooks", "ebooks", "processing", "failed"} {
		if err := os.MkdirAll(filepath.Join(libraryRoot, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}
	}
	incoming := filepath.Join(libraryRoot, "incoming")
	if err := os.MkdirAll(incoming, 0o777); err != nil {
		return nil, fmt.Errorf("create incoming: %w", err)
	}
	// MkdirAll respects the process umask, so explicitly chmod to make sure
	// the host user can write into incoming/ regardless of the container's
	// umask setting.
	if err := os.Chmod(incoming, 0o777); err != nil {
		log.Printf("ingest queue: warning, could not chmod incoming to 0777: %v", err)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := fsw.Add(filepath.Join(libraryRoot, "incoming")); err != nil {
		fsw.Close()
		return nil, err
	}

	return &IngestQueue{
		libraryRoot: libraryRoot,
		watcher:     fsw,
		stopCh:      make(chan struct{}),
		pending:     make(map[string]time.Time),
	}, nil
}

func (q *IngestQueue) Start() {
	go q.eventLoop()
	go q.pollLoop()
	log.Printf("ingest queue watching %s/incoming", q.libraryRoot)
	// Immediately sweep anything left over from a prior run.
	q.scanIncoming()
}

func (q *IngestQueue) Close() error {
	close(q.stopCh)
	return q.watcher.Close()
}

// eventLoop turns fsnotify events into "this path may have settled — check
// it later" entries. Actual work happens in scanIncoming.
func (q *IngestQueue) eventLoop() {
	for {
		select {
		case <-q.stopCh:
			return
		case ev, ok := <-q.watcher.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
				q.mu.Lock()
				q.pending[ev.Name] = time.Now()
				q.mu.Unlock()
			}
		case err := <-q.watcher.Errors:
			log.Printf("ingest queue watcher error: %v", err)
		}
	}
}

// pollLoop also runs scanIncoming on a timer so that:
//   - Files that arrive without fsnotify events (some network filesystems)
//     still get picked up.
//   - Unfinished files that needed to settle get checked again.
func (q *IngestQueue) pollLoop() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-t.C:
			q.scanIncoming()
		}
	}
}

// scanIncoming lists everything currently in incoming/ and processes anything
// that looks stable (file size hasn't changed in stableQuietPeriod).
func (q *IngestQueue) scanIncoming() {
	incoming := filepath.Join(q.libraryRoot, "incoming")
	entries, err := os.ReadDir(incoming)
	if err != nil {
		log.Printf("ingest queue: read incoming: %v", err)
		return
	}
	for _, e := range entries {
		// Skip hidden / dotfiles (covers .DS_Store, partial files, etc.)
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(incoming, e.Name())
		if !q.isStable(path) {
			continue
		}
		if err := q.process(path); err != nil {
			log.Printf("ingest queue: %s: %v", e.Name(), err)
		}
	}
}

// isStable returns true if path's modification time is at least
// stableQuietPeriod in the past. For directories, walks recursively and
// requires every file to be stable.
func (q *IngestQueue) isStable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	now := time.Now()
	if !info.IsDir() {
		return now.Sub(info.ModTime()) >= stableQuietPeriod
	}
	// Directory: every file inside must be stable.
	stable := true
	filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if now.Sub(fi.ModTime()) < stableQuietPeriod {
			stable = false
		}
		return nil
	})
	return stable
}

// process handles a single incoming entry: classify, copy to library,
// move to failed/ on permanent error. Retries handled via a sidecar
// .queue.json file kept inside processing/.
func (q *IngestQueue) process(path string) error {
	name := filepath.Base(path)
	jobDir := filepath.Join(q.libraryRoot, "processing", name)

	// Atomic move from incoming/ to processing/. If the destination already
	// exists (we crashed mid-job earlier), pick up where we left off.
	if _, err := os.Stat(jobDir); os.IsNotExist(err) {
		if err := os.Rename(path, jobDir); err != nil {
			return fmt.Errorf("move to processing: %w", err)
		}
	}

	meta, err := loadOrInitMeta(jobDir, name)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	dst, kind, err := q.classify(jobDir)
	if err != nil {
		return q.fail(jobDir, meta, fmt.Errorf("classify: %w", err))
	}

	log.Printf("ingest queue: %s → %s/%s", name, kind, filepath.Base(dst))

	if err := copyAny(jobDir, dst); err != nil {
		meta.Retries++
		meta.LastError = err.Error()
		saveMeta(jobDir, meta)
		if meta.Retries >= maxRetries {
			return q.fail(jobDir, meta, fmt.Errorf("after %d retries: %w", maxRetries, err))
		}
		log.Printf("ingest queue: %s: copy failed (%d/%d): %v", name, meta.Retries, maxRetries, err)
		q.notify()
		return nil
	}

	// Success. Clean up the processing entry.
	os.RemoveAll(jobDir)
	log.Printf("ingest queue: %s ingested into %s/", name, kind)
	q.notify()
	return nil
}

func (q *IngestQueue) notify() {
	if q.onChange != nil {
		q.onChange()
	}
}

// classify decides whether the source should land in audiobooks/ or ebooks/
// and returns the target path (without yet creating it). For directories,
// looks at the file extensions inside.
func (q *IngestQueue) classify(src string) (target, kind string, err error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", "", err
	}

	if info.IsDir() {
		// Look at children — if any are audio, treat the whole dir as audiobook.
		var hasAudio, hasText bool
		entries, err := os.ReadDir(src)
		if err != nil {
			return "", "", err
		}
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if audioExts[ext] {
				hasAudio = true
			}
			if ext == ".epub" || ext == ".pdf" || ext == ".mobi" || ext == ".azw3" || ext == ".txt" {
				hasText = true
			}
		}
		if hasAudio {
			return filepath.Join(q.libraryRoot, "audiobooks", filepath.Base(src)), "audiobooks", nil
		}
		if hasText {
			return filepath.Join(q.libraryRoot, "ebooks", filepath.Base(src)), "ebooks", nil
		}
		return "", "", fmt.Errorf("directory %q contains no recognized audio or ebook files", filepath.Base(src))
	}

	ext := strings.ToLower(filepath.Ext(src))
	if audioExts[ext] {
		return filepath.Join(q.libraryRoot, "audiobooks", filepath.Base(src)), "audiobooks", nil
	}
	if ext == ".epub" || ext == ".pdf" || ext == ".mobi" || ext == ".azw3" || ext == ".txt" {
		return filepath.Join(q.libraryRoot, "ebooks", filepath.Base(src)), "ebooks", nil
	}
	if ext == ".json" && strings.HasSuffix(src, ".stt.json") {
		// A sidecar — should land next to the matching audiobook in audiobooks/.
		return filepath.Join(q.libraryRoot, "audiobooks", filepath.Base(src)), "audiobooks", nil
	}
	return "", "", fmt.Errorf("unsupported file type %q (ext %s)", filepath.Base(src), ext)
}

// fail moves a processing/ entry into failed/ with the metadata showing the
// last error. Returns the original error so callers can log it once.
func (q *IngestQueue) fail(jobDir string, meta queueMeta, cause error) error {
	failedDir := filepath.Join(q.libraryRoot, "failed", filepath.Base(jobDir))
	// If something already exists at failed/<name>, suffix with timestamp.
	if _, err := os.Stat(failedDir); err == nil {
		failedDir = failedDir + "." + time.Now().Format("20060102-150405")
	}
	if err := os.Rename(jobDir, failedDir); err != nil {
		return fmt.Errorf("move to failed: %w (original: %v)", err, cause)
	}
	meta.LastError = cause.Error()
	saveMeta(failedDir, meta)
	log.Printf("ingest queue: gave up on %s → failed/: %v", meta.Original, cause)
	q.notify()
	return cause
}

// --- helpers ---

func loadOrInitMeta(jobDir, original string) (queueMeta, error) {
	metaPath := filepath.Join(jobDir, ".queue.json")
	if data, err := os.ReadFile(metaPath); err == nil {
		var m queueMeta
		if err := json.Unmarshal(data, &m); err == nil {
			return m, nil
		}
	}
	return queueMeta{
		JobID:     fmt.Sprintf("%d", time.Now().UnixNano()),
		Original:  original,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func saveMeta(jobDir string, m queueMeta) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	// Ensure parent exists in case we just moved to failed/.
	os.MkdirAll(jobDir, 0o755)
	return os.WriteFile(filepath.Join(jobDir, ".queue.json"), data, 0o644)
}

// copyAny copies a file or directory tree from src to dst. If dst already
// exists, the existing data is left in place — useful when an earlier run
// crashed mid-copy. Skips .queue.json so metadata doesn't pollute the
// canonical library.
func copyAny(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst)
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".queue.json" {
			continue
		}
		if err := copyAny(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
