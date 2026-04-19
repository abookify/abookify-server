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

	// Decide which chapter list to use on the audio book. Priority:
	//   1. Narrator-pattern chapters from sidecar, if reliable (prior check).
	//   2. Pause-based detection (word gaps >= CHAPTER_PAUSE_SECS) when the
	//      sidecar's chapter list is missing or unreliable.
	// Pause-based is a last resort — it depends purely on audio gaps, so for
	// a tightly-edited audiobook it can still over- or under-segment.
	audioChapters := sc.Chapters
	if !chaptersLookReliable(sc.Chapters, sc.Duration) {
		paused := detectChaptersFromPauses(sc.Words)
		if len(paused) > 1 {
			log.Printf("sidecar-import: pause-based detection found %d chapter boundaries (narrator-pattern was unreliable)", len(paused))
			audioChapters = paused
		} else {
			audioChapters = nil
		}
	}
	if len(audioChapters) > 0 {
		store.DeleteChaptersByBook(audioBookID)
		for i, ch := range audioChapters {
			end := ch.End
			if end <= ch.Start {
				if i+1 < len(audioChapters) {
					end = audioChapters[i+1].Start
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
	// Prefer narrator-pattern detection if reliable. Otherwise use pause
	// detection from word gaps (no ffmpeg — just read the existing timestamps).
	chaptersToUse := sc.Chapters
	reliable := chaptersLookReliable(sc.Chapters, sc.Duration)
	useChapters := len(sc.Chapters) > 0 && reliable
	if !useChapters {
		paused := detectChaptersFromPauses(sc.Words)
		// Only adopt pause-based chapters if they produce plausible counts.
		if len(paused) >= 3 && len(paused) <= 80 {
			chaptersToUse = paused
			useChapters = true
			log.Printf("sidecar-import: using pause-based chapters (%d found) on text book",
				len(paused))
		}
	}
	if useChapters {
		// If the first detected chapter doesn't start at the beginning of the
		// book, prepend a "Prelude" covering [0, firstStart]. Without this,
		// the first hour+ of audio has no readable text when narrator-pattern
		// detection missed the intro.
		firstStart := chaptersToUse[0].Start
		firstWordIdx := chaptersToUse[0].WordIdx
		if firstStart > 60 || firstWordIdx > 100 {
			ranges = append(ranges, sttChapter{
				Title:   "Prelude",
				Start:   0,
				End:     firstStart,
				WordIdx: 0,
			})
		}
		for i, ch := range chaptersToUse {
			end := ch.End
			if end <= ch.Start {
				if i+1 < len(chaptersToUse) {
					end = chaptersToUse[i+1].Start
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

	// Accumulate paragraphs across all chapters and write in one atomic
	// transaction after the chapter loop — ReplaceParagraphsForBook deletes
	// all existing paragraphs first, so per-chapter calls would wipe each
	// other's work.
	allParas := []db.Paragraph{}

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

		// Build paragraph rows from pause detection. These let the FE render
		// the chapter as real paragraphs and only wrap sync-spans for the
		// active one (bounded DOM cost regardless of chapter size).
		pRanges := detectParagraphsFromPauses(sc.Words, wStart, wEnd)
		for pi, pr := range pRanges {
			// pr.start/end are chapter-local word indices.
			text := buildChapterContentByIdx(sc.Words, wStart+pr.start, wStart+pr.end)
			if text == "" {
				continue
			}
			allParas = append(allParas, db.Paragraph{
				BookID:       textBookID,
				ChapterIdx:   idx,
				ParagraphIdx: pi,
				WordStart:    pr.start,
				WordEnd:      pr.end,
				Text:         text,
			})
		}
	}

	if len(allParas) > 0 {
		if err := store.ReplaceParagraphsForBook(textBookID, allParas); err != nil {
			log.Printf("sidecar-import: paragraph insert failed for book %d: %v", textBookID, err)
		}
	}

	return nil
}

// Pause thresholds for post-processing the word timestamp stream. Tuned
// against 438 Days, Why We Sleep, and PHM. No audio re-processing needed —
// we just read gaps in the existing sidecar data.
//
// - CHAPTER_PAUSE_SECS: a really long gap, almost always a chapter boundary
//   (includes silence + the narrator resetting + production edits).
// - PARAGRAPH_PAUSE_SECS: a medium gap, typical sentence-break-plus-breath
//   but also used for soft paragraph divisions in audiobooks.
const (
	CHAPTER_PAUSE_SECS   = 3.0
	PARAGRAPH_PAUSE_SECS = 0.6
)

// detectChaptersFromPauses walks the word timestamps and flags indexes where
// the gap to the next word exceeds CHAPTER_PAUSE_SECS. These are candidate
// chapter starts. Titles are inferred from the first few words after each
// boundary — looking for "Chapter N", "Part N", "Foreword", etc. — so the
// TOC reads meaningfully instead of just "Chapter 1, 2, 3…".
func detectChaptersFromPauses(words []sttWord) []sttChapter {
	if len(words) < 2 {
		return nil
	}
	var chapters []sttChapter
	// Boundary starts: [0, i1, i2, …] where each i is the first word after
	// a chapter-sized gap.
	starts := []int{0}
	for i := 0; i < len(words)-1; i++ {
		gap := words[i+1].Start - words[i].End
		if gap >= CHAPTER_PAUSE_SECS {
			starts = append(starts, i+1)
		}
	}
	for n, s := range starts {
		chapters = append(chapters, sttChapter{
			Title:   inferChapterTitle(words, s, n+1),
			Start:   words[s].Start,
			WordIdx: s,
		})
	}
	return chapters
}

// Section-type prefix words we recognise at the start of a chapter. Keep
// this list conservative — a recognized prefix becomes the chapter title,
// so false positives produce mislabels.
var sectionPrefixes = []string{
	"chapter", "part", "book", "section",
	"prologue", "epilogue", "foreword", "afterword", "introduction",
	"preface", "dedication", "acknowledgments", "acknowledgements",
}

// TITLE_PAUSE_SECS is the gap size we treat as the end of a chapter title.
// Narrators typically pause between "Chapter Two" and the title proper, and
// again between the title and the first sentence of body text. 0.6s matches
// the paragraph-break threshold which is roughly the same acoustic signature.
const TITLE_PAUSE_SECS = 0.6

// inferChapterTitle reads the first ~15 words starting at `start` and
// returns a human-sounding chapter title. Detection priority:
//  1. "Chapter|Part|Book N[: Subtitle]" → keep through the first major pause
//     or end-of-sentence punctuation, whichever comes first.
//  2. "Prologue", "Foreword", "Introduction", etc. → use the word.
//  3. Else → short snippet (≤8 words) for TOC display.
//
// The pause-based cut is important for audiobooks where Whisper doesn't
// reliably punctuate the announcement. In "Chapter Two [1.5s pause]
// Caffeine, Jet Lag and Melatonin [0.8s pause] Losing and Gaining…" the
// period-based cutoff alone produces a runon ("Chapter Two Caffeine, Jet
// Lag and Melatonin Losing and Gaining Control…") because Whisper often
// skips the header-to-body period.
func inferChapterTitle(words []sttWord, start, chapNum int) string {
	const peek = 20
	end := start + peek
	if end > len(words) {
		end = len(words)
	}

	// Gather tokens from start up to the first pause >= TITLE_PAUSE_SECS.
	// That gap is almost always the narrator's breath between title and body.
	var rawTokens, displayTokens []string
	cutAt := end
	for i := start; i < end; i++ {
		t := strings.TrimSpace(words[i].Word)
		if t != "" {
			rawTokens = append(rawTokens, t)
			displayTokens = append(displayTokens, words[i].Word)
		}
		// Look ahead at the gap to word i+1. If it's a significant pause
		// AND we're past the first couple of words (so "Chapter" [pause] "N"
		// doesn't cut off the number), this is our title boundary.
		if i+1 < end && i > start+1 {
			gap := words[i+1].Start - words[i].End
			if gap >= TITLE_PAUSE_SECS {
				cutAt = i + 1
				break
			}
		}
	}
	_ = cutAt // reserved for future use if we extend past the peek window

	if len(rawTokens) == 0 {
		return fmt.Sprintf("Chapter %d", chapNum)
	}
	lower := strings.ToLower(strings.Join(rawTokens, " "))

	// Case 1: "chapter/part/book N…" with optional subtitle.
	// Cut at period/exclaim/question (sentence end) OR at the pause already
	// captured by our token window. Keep colon (subtitles like
	// "Part One: This Thing Called Sleep" are exactly what we want).
	for _, prefix := range []string{"chapter", "part", "book"} {
		if strings.HasPrefix(lower, prefix+" ") {
			text := strings.TrimSpace(strings.Join(displayTokens, ""))
			if idx := strings.IndexAny(text, ".!?"); idx > 0 && idx < 80 {
				text = text[:idx]
			} else if len(text) > 80 {
				text = text[:80] + "…"
			}
			return strings.TrimSpace(text)
		}
	}

	// Case 2: single-word section marker.
	firstLower := strings.ToLower(rawTokens[0])
	for _, p := range sectionPrefixes {
		if firstLower == p {
			return strings.ToUpper(p[:1]) + p[1:]
		}
	}

	// Case 3: snippet fallback. The pause-based tokenization above already
	// capped the snippet at the first significant pause, so this typically
	// gives a clean opening phrase rather than a runon.
	snippet := rawTokens
	if len(snippet) > 8 {
		snippet = snippet[:8]
	}
	suffix := "…"
	if len(rawTokens) <= 8 && cutAt < end {
		// Full natural phrase fit without a pause; no ellipsis needed.
		suffix = ""
	}
	return fmt.Sprintf("Ch %d · %s%s", chapNum, strings.Join(snippet, " "), suffix)
}

// detectParagraphsFromPauses walks the words in [start, end) and flags
// paragraph boundaries at gaps exceeding PARAGRAPH_PAUSE_SECS. Returns
// chapter-local word-index ranges.
func detectParagraphsFromPauses(words []sttWord, start, end int) []idxRange {
	if end <= start || end > len(words) {
		return nil
	}
	var paras []idxRange
	paraStart := start
	for i := start; i < end-1; i++ {
		gap := words[i+1].Start - words[i].End
		if gap >= PARAGRAPH_PAUSE_SECS {
			paras = append(paras, idxRange{paraStart - start, i + 1 - start})
			paraStart = i + 1
		}
	}
	paras = append(paras, idxRange{paraStart - start, end - start})
	return paras
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
//
// Also inserts paragraph breaks (double newlines) at gaps exceeding
// PARAGRAPH_PAUSE_SECS. These breaks let the FE render the transcript as
// real paragraphs and wrap sync-spans on-demand per paragraph (bounded DOM
// cost regardless of chapter size).
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
		// Insert a paragraph break on a significant pause (but not after the
		// last word of the chunk — that would add a trailing blank line).
		if i+1 < end {
			gap := words[i+1].Start - words[i].End
			if gap >= PARAGRAPH_PAUSE_SECS {
				b.WriteString("\n\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}
