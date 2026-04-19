// sidecar_import.go — automatic import of .stt.json sidecars produced by
// stt-cli. When the scanner finds audiobook directories that already have
// transcription sidecars (from an earlier CLI run or rsync'd from another
// machine), we import the word timestamps + detected chapters directly into
// the database so the karaoke pipeline is immediately usable — no need to
// re-run Whisper.
package library

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// sttSidecar is the JSON structure written by stt-cli.
type sttSidecar struct {
	Language string           `json:"language"`
	Duration float64          `json:"duration"`
	Text     string           `json:"text"`
	Words    []sttWord        `json:"words"`
	Chapters []sttChapter     `json:"chapters"`
	Sources  []sttSource      `json:"sources"`
}

// Note the compact field names. stt-cli emits sync data in this shape
// (s/e/w) to keep the JSON size manageable — 70k+ words adds up fast.
type sttWord struct {
	Start float64 `json:"s"`
	End   float64 `json:"e"`
	Word  string  `json:"w"`
}

type sttChapter struct {
	Title     string  `json:"title"`
	Start     float64 `json:"start_sec"`
	End       float64 `json:"end_sec"`
	WordIdx   int     `json:"word_idx"`   // stt-cli emits this for precise chapter slicing
	WordCount int     `json:"word_count"`
}

type sttSource struct {
	File       string  `json:"file"`
	OffsetSecs float64 `json:"offset_secs"`
	Duration   float64 `json:"duration_secs"`
}

// ImportSidecars scans the library root for .stt.json files next to audiobook
// directories or audio files. For each sidecar found, it imports word
// timestamps into sync_data and detected chapters into the chapters table.
// Idempotent: skips works that already have sync_data.
func ImportSidecars(store *db.Store, libraryRoot string) {
	works, err := store.ListWorks()
	if err != nil {
		log.Printf("sidecar-import: failed to list works: %v", err)
		return
	}

	imported := 0
	importedPaths := map[string]bool{} // deduplicate: one sidecar = one import
	for _, w := range works {
		if !w.HasAudio {
			continue
		}
		// Use the first audio book as the canonical target for the sidecar.
		// Multi-file audiobooks have a dir-level sidecar that covers the
		// whole concatenated timeline — importing once on the first book is
		// correct; importing per-book would duplicate the data N times.
		af := w.AudioFiles[0]

		// Already has sync data? Skip.
		existing, _ := store.GetSyncData(w.ID, af.ID, 0)
		if existing != "" && existing != "[]" {
			continue
		}

		sidecarPath := findSidecar(af.Path, libraryRoot)
		if sidecarPath == "" || importedPaths[sidecarPath] {
			continue
		}
		importedPaths[sidecarPath] = true

		if err := importOneSidecar(store, w.ID, af.ID, sidecarPath); err != nil {
			log.Printf("sidecar-import: %s: %v", filepath.Base(sidecarPath), err)
			continue
		}
		imported++
	}
	if imported > 0 {
		log.Printf("sidecar-import: imported %d sidecar(s)", imported)
	}
}

// findSidecar locates a .stt.json sidecar for the given book path.
// Tries:
//   1. <parent-dir>.stt.json (multi-file audiobooks)
//   2. <file>.stt.json (single-file audiobooks, replacing the original extension)
//   3. <file-without-ext>.stt.json
func findSidecar(bookPath, libraryRoot string) string {
	// bookPath is the container path (/library/audiobooks/...). Map back to
	// the host filesystem so we can stat.
	hostPath := bookPath
	if strings.HasPrefix(hostPath, "/library/") {
		hostPath = filepath.Join(libraryRoot, hostPath[len("/library/"):])
	}

	// Try parent directory sidecar: /path/to/Dir.stt.json
	dir := filepath.Dir(hostPath)
	dirSidecar := dir + ".stt.json"
	if fileExists(dirSidecar) {
		return dirSidecar
	}

	// Try file sidecar: /path/to/book.mp3 → /path/to/book.stt.json
	noExt := strings.TrimSuffix(hostPath, filepath.Ext(hostPath))
	fileSidecar := noExt + ".stt.json"
	if fileExists(fileSidecar) {
		return fileSidecar
	}

	// Try full-name sidecar: /path/to/book.mp3.stt.json
	fullSidecar := hostPath + ".stt.json"
	if fileExists(fullSidecar) {
		return fullSidecar
	}

	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func importOneSidecar(store *db.Store, workID, audioBookID int64, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read sidecar: %w", err)
	}

	var sc sttSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return fmt.Errorf("parse sidecar: %w", err)
	}

	if len(sc.Words) == 0 {
		return fmt.Errorf("no words in sidecar")
	}

	// Build sync_data JSON: [{s, e, w}, ...]
	type syncWord struct {
		S float64 `json:"s"`
		E float64 `json:"e"`
		W string  `json:"w"`
	}
	syncWords := make([]syncWord, len(sc.Words))
	for i, w := range sc.Words {
		syncWords[i] = syncWord{S: w.Start, E: w.End, W: w.Word}
	}
	tsJSON, _ := json.Marshal(syncWords)
	if err := store.SaveSyncData(workID, audioBookID, 0, string(tsJSON)); err != nil {
		return fmt.Errorf("save sync_data: %w", err)
	}

	// Import detected chapters (if any). Backfill end_sec from successive
	// start_sec values when the sidecar omits it.
	if len(sc.Chapters) > 0 {
		store.DeleteChaptersByBook(audioBookID)
		for i, ch := range sc.Chapters {
			end := ch.End
			if end <= ch.Start {
				if i+1 < len(sc.Chapters) {
					end = sc.Chapters[i+1].Start
				} else if sc.Duration > 0 {
					end = sc.Duration
				} else {
					end = ch.Start + 3600 // fallback
				}
			}
			title := ch.Title
			if title == "" {
				title = fmt.Sprintf("Chapter %d", i+1)
			}
			store.InsertChapter(db.Chapter{
				BookID:     audioBookID,
				Index:      i,
				Title:      title,
				Src:        "transcript",
				StartSec:   ch.Start,
				EndSec:     end,
				Confidence: 0.9,
				WordCount:  ch.WordCount,
			})
		}
	}

	// Create a transcript "text" book and chapter rows so the reader has
	// something to render — without this the sync_data is orphaned and the
	// work shows up as audio-only with no karaoke surface. Mirrors the shape
	// 438 Days has after the normal STT → transcript-split pipeline.
	if err := ensureTranscriptBook(store, workID, audioBookID, &sc); err != nil {
		log.Printf("sidecar-import: transcript book creation failed: %v", err)
	}

	log.Printf("sidecar-import: work=%d book=%d words=%d chapters=%d (%s)",
		workID, audioBookID, len(sc.Words), len(sc.Chapters), filepath.Base(path))
	return nil
}

// ensureTranscriptBook creates a synthetic text book holding the sidecar's
// transcript, split into chapters by the detected boundaries. Idempotent:
// if a transcript book already exists for this work, it's updated in place.
func ensureTranscriptBook(store *db.Store, workID, audioBookID int64, sc *sttSidecar) error {
	transcriptPath := fmt.Sprintf("generated://transcript/work-%d", workID)
	title := fmt.Sprintf("Transcript (work %d)", workID)

	// Find existing work to use its title if available.
	if w, err := store.GetWork(workID); err == nil && w != nil {
		title = w.Title + " (Transcript)"
	}

	b := db.Book{
		WorkID:     workID,
		Path:       transcriptPath,
		Filename:   title,
		Format:     "transcript",
		MediaType:  "text",
		Title:      title,
		Origin:     "whisper_transcript",
		Visibility: "visible",
	}
	if err := store.UpsertBook(b); err != nil {
		return fmt.Errorf("upsert transcript book: %w", err)
	}

	// Look up the inserted book's ID.
	books, err := store.ListBooks()
	if err != nil {
		return fmt.Errorf("list books: %w", err)
	}
	var textBookID int64
	for _, bk := range books {
		if bk.Path == transcriptPath {
			textBookID = bk.ID
			break
		}
	}
	if textBookID == 0 {
		return fmt.Errorf("transcript book not found after upsert")
	}

	// Wipe existing chapters + repopulate. Cheap enough for one-shot import.
	store.DeleteChaptersByBook(textBookID)

	// Build chapter content by slicing the words array by timestamp range.
	// If no chapter boundaries, write a single "Full Transcript" chapter.
	ranges := []sttChapter{}
	useChapters := len(sc.Chapters) > 0 && chaptersLookReliable(sc.Chapters, sc.Duration)
	if useChapters {
		// If the first detected chapter doesn't start at the beginning of the
		// book (a common failure mode of narrator-pattern chapter detection —
		// it misses the intro and picks up the first "chapter X" the narrator
		// mentions mid-book), prepend a "Prelude" covering [0, firstStart].
		// Without this, the first hour+ of audio has no readable text.
		firstStart := sc.Chapters[0].Start
		firstWordIdx := sc.Chapters[0].WordIdx
		if firstStart > 60 || firstWordIdx > 100 {
			ranges = append(ranges, sttChapter{
				Title:   "Prelude",
				Start:   0,
				End:     firstStart,
				WordIdx: 0,
			})
		}
		// Backfill end_sec so ranges are well-formed.
		for i, ch := range sc.Chapters {
			end := ch.End
			if end <= ch.Start {
				if i+1 < len(sc.Chapters) {
					end = sc.Chapters[i+1].Start
				} else if sc.Duration > 0 {
					end = sc.Duration
				} else {
					end = ch.Start + 3600
				}
			}
			title := ch.Title
			if title == "" {
				title = fmt.Sprintf("Chapter %d", i+1)
			}
			ranges = append(ranges, sttChapter{
				Title:     title,
				Start:     ch.Start,
				End:       end,
				WordIdx:   ch.WordIdx,
				WordCount: ch.WordCount,
			})
		}
	} else {
		// Single-chapter fallback covering the whole book.
		ranges = append(ranges, sttChapter{
			Title:   "Full Transcript",
			Start:   0,
			End:     sc.Duration + 1,
			WordIdx: 0,
		})
	}

	// Build word-index ranges for each chapter. Prefer explicit word_idx
	// from the sidecar (precise, no boundary words missed); fall back to
	// timestamp-range when word_idx isn't provided.
	wordIdxRanges := buildWordIdxRanges(sc, ranges)

	for idx, r := range ranges {
		wStart, wEnd := wordIdxRanges[idx].start, wordIdxRanges[idx].end
		content := buildChapterContentByIdx(sc.Words, wStart, wEnd)
		wordCount := 0
		if content != "" {
			wordCount = len(strings.Fields(content))
		}
		store.InsertChapter(db.Chapter{
			BookID:     textBookID,
			Index:      idx,
			Title:      r.Title,
			Src:        "transcript",
			Content:    content,
			WordCount:  wordCount,
			StartSec:   r.Start,
			EndSec:     r.End,
			Confidence: 0.9,
		})
	}

	return nil
}

// chaptersLookReliable applies heuristics to decide whether the sidecar's
// chapter detection is trustworthy. Returns false (with log) when red flags
// suggest the detector picked up narrator cross-references rather than real
// chapter boundaries.
//
// Red flags:
//   - Duplicate titles ("Part 3" x2) — real chapters have unique names.
//   - First chapter starts >20% into the book — detector missed the real intro.
//   - Very few chapters relative to duration (< 1 per hour for books >3h) —
//     most audiobooks have ~15-30 min per chapter.
//   - Non-monotonic chapter numbers where extractable (Chapter 11 before Chapter 6).
func chaptersLookReliable(chapters []sttChapter, duration float64) bool {
	if len(chapters) == 0 {
		return false
	}

	// 1. Duplicate titles.
	seen := map[string]int{}
	for _, ch := range chapters {
		seen[strings.ToLower(ch.Title)]++
	}
	for t, c := range seen {
		if c > 1 {
			log.Printf("sidecar-import: chapter detection looks unreliable — duplicate title %q (%dx), falling back to full transcript", t, c)
			return false
		}
	}

	// 2. First chapter starts >20% into the book.
	if duration > 0 && chapters[0].Start > duration*0.20 {
		log.Printf("sidecar-import: chapter detection looks unreliable — first chapter starts at %.0fs (%.0f%% into a %.0fs book), falling back to full transcript",
			chapters[0].Start, (chapters[0].Start/duration)*100, duration)
		return false
	}

	// 3. Too few chapters for a long book (< 1 per 90 min for books >3h).
	if duration > 3*3600 {
		minExpected := int(duration / (90 * 60))
		if len(chapters) < minExpected {
			log.Printf("sidecar-import: chapter detection looks unreliable — only %d chapters for a %.1fh book (expected at least %d), falling back to full transcript",
				len(chapters), duration/3600, minExpected)
			return false
		}
	}

	return true
}

type idxRange struct{ start, end int }

// buildWordIdxRanges converts sidecar chapter boundaries into [start,end)
// word-index ranges. Uses explicit word_idx when present; otherwise scans
// the words array for the first word at or past each chapter's start_sec.
func buildWordIdxRanges(sc *sttSidecar, ranges []sttChapter) []idxRange {
	out := make([]idxRange, len(ranges))
	total := len(sc.Words)

	if len(sc.Chapters) == len(ranges) && len(sc.Chapters) > 0 && sc.Chapters[0].WordIdx > 0 {
		// Trust explicit word_idx from stt-cli.
		for i, ch := range sc.Chapters {
			out[i].start = ch.WordIdx
			if i+1 < len(sc.Chapters) {
				out[i].end = sc.Chapters[i+1].WordIdx
			} else {
				out[i].end = total
			}
		}
		return out
	}

	// Fallback: scan timestamps. Binary-search-style sweep with a running
	// pointer since words are time-ordered.
	wi := 0
	for i, r := range ranges {
		for wi < total && sc.Words[wi].Start < r.Start {
			wi++
		}
		out[i].start = wi
	}
	// End of chapter i = start of chapter i+1 (or end of array).
	for i := range out {
		if i+1 < len(out) {
			out[i].end = out[i+1].start
		} else {
			out[i].end = total
		}
	}
	return out
}

// buildChapterContentByIdx joins the word texts at [start, end). Leading
// spaces from Whisper tokens are preserved since that matches natural prose
// spacing and punctuation attachment.
func buildChapterContentByIdx(words []sttWord, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(words) {
		end = len(words)
	}
	if start >= end {
		return ""
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(words[i].Word)
	}
	return strings.TrimSpace(b.String())
}
