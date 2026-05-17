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
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// sttSidecar is the JSON structure written by stt-cli. Handles both v1
// (no "version" field) and v2 (version=2, with silences + events). v2
// carries real acoustic silence events from ffmpeg silencedetect as
// ground truth for chapter/paragraph boundaries.
type sttSidecar struct {
	Version  int            `json:"version"`
	Language string         `json:"language"`
	Duration float64        `json:"duration"`
	Text     string         `json:"text"`
	Words    []sttWord      `json:"words"`
	Silences []sttSilence   `json:"silences"` // v2 only
	Chapters []sttChapter   `json:"chapters"`
	Sources  []sttSource    `json:"sources"`
}

// Note the compact field names. stt-cli emits sync data in this shape
// (s/e/w) to keep the JSON size manageable — 70k+ words adds up fast.
type sttWord struct {
	Start       float64 `json:"s"`
	End         float64 `json:"e"`
	Word        string  `json:"w"`
	Probability float64 `json:"conf,omitempty"` // v2 only (Whisper confidence)
}

// sttSilence is an independently-measured acoustic silence event (v2).
// Source is "silencedetect" or "vad" or "both".
type sttSilence struct {
	Start    float64 `json:"s"`
	End      float64 `json:"e"`
	Duration float64 `json:"duration"`
	Source   string  `json:"source"`
	RmsDB    float64 `json:"rms_db,omitempty"`
	Kind     string  `json:"kind"` // "chapter" | "paragraph" | "sentence" | "breath"
}

type sttChapter struct {
	Title     string  `json:"title"`
	Start     float64 `json:"start_sec"`
	End       float64 `json:"end_sec"`
	WordIdx   int     `json:"word_idx"`   // stt-cli emits this for precise chapter slicing
	WordCount int     `json:"word_count"`
	Src       string  `json:"src,omitempty"` // "part" | "chapter" (empty = chapter by default)
}

type sttSource struct {
	File       string  `json:"file"`
	OffsetSecs float64 `json:"offset_secs"`
	Duration   float64 `json:"duration_secs"`
}

// isV2 reports whether the sidecar uses the v2 schema (with silence events).
func (s *sttSidecar) isV2() bool { return s.Version >= 2 && len(s.Silences) > 0 }

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
	//   1. Sidecar's own chapters, if marked reliable.
	//   2. Narrator-pattern (DetectChapters) — substring-match "Chapter N"
	//      announcements in the transcript. Authoritative when the narrator
	//      explicitly announces chapters; uses silence boost only as a
	//      tiebreaker for orphan candidates.
	//   3. v2 silence events — ground-truth acoustic pauses. Used when the
	//      narrator doesn't announce chapters (Tortilla Flat, etc.).
	//   4. Pause-based word-gap detection — last-resort fallback for v1
	//      sidecars of books with no spoken chapter announcements.
	audioChapters := sc.Chapters
	if len(audioChapters) > 0 && !chaptersLookReliable(audioChapters, sc.Duration) {
		audioChapters = nil
	}
	if len(audioChapters) == 0 {
		syncWords := sttToSyncTimestamps(sc.Words)
		detected := DetectChapters(syncWords, sc.Duration)
		if narratorRunIsStrong(detected, sc.Duration) {
			log.Printf("sidecar-import: DetectChapters found %d narrator-pattern %s chapters (strong run)", len(detected), detected[0].Kind)
			// Meta-pass: if numbering has gaps (e.g. narrator-pattern returned
			// 1-13 + 16-31, missing 14 and 15), insert inferred chapters at
			// chapter-grade silences in the gap so the user-facing TOC is
			// continuous. Without this, the UI would jump from "Chapter 13"
			// to "Chapter 16" with no explanation.
			detected = fillNumberingGaps(detected, &sc)
			audioChapters = detectedToSttChapters(sc.Words, sc.Silences, detected)
		}
	}
	if len(audioChapters) == 0 && sc.isV2() {
		silChapters := detectChaptersFromSilences(&sc)
		parts := detectPartsFromSilences(&sc)
		if len(silChapters) >= 3 {
			merged := mergePartsAndChapters(parts, silChapters)
			log.Printf("sidecar-import: v2 silence-based detection found %d entries (%d chapters, %d parts)",
				len(merged), len(silChapters), len(parts))
			audioChapters = merged
		}
	}
	if len(audioChapters) == 0 {
		// Last resort: pause detection only.
		paused := detectChaptersFromPauses(sc.Words)
		if len(paused) > 1 {
			log.Printf("sidecar-import: pause-based detection found %d chapter boundaries", len(paused))
			audioChapters = paused
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
			// Tag parts so the UI can render them as section headers.
			src := "transcript"
			if ch.Src == "part" {
				src = "part"
			}
			store.InsertChapter(db.Chapter{
				BookID:     audioBookID,
				Index:      i,
				Title:      title,
				Src:        src,
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
	// Chapter source priority (same as audio book):
	//   1. Reliable sidecar narrator-pattern chapters.
	//   2. DetectChapters — narrator-pattern run from the word stream.
	//      Authoritative when the narrator says "Chapter N".
	//   3. v2 silence events — ground truth pauses. Used when no narrator
	//      announcements (Tortilla Flat, etc.).
	//   4. Pause-based fallback for v1.
	chaptersToUse := sc.Chapters
	reliable := chaptersLookReliable(sc.Chapters, sc.Duration)
	useChapters := len(sc.Chapters) > 0 && reliable

	if !useChapters {
		syncWords := sttToSyncTimestamps(sc.Words)
		detected := DetectChapters(syncWords, sc.Duration)
		if narratorRunIsStrong(detected, sc.Duration) {
			// Same meta-pass as the audio path: fill any gaps in the
			// numbering with inferred entries so the audio and text TOCs
			// align 1:1. Without this, the audio book would have Chapters
			// 1-31 (gaps filled) but the text book would have only the
			// announced 29 — chapter-link would mismatch.
			detected = fillNumberingGaps(detected, sc)
			chaptersToUse = detectedToSttChapters(sc.Words, sc.Silences, detected)
			useChapters = true
			log.Printf("sidecar-import: text book using DetectChapters narrator-pattern (%d %ss, strong run)", len(detected), detected[0].Kind)
		}
	}

	if !useChapters && sc.isV2() {
		silChapters := detectChaptersFromSilences(sc)
		parts := detectPartsFromSilences(sc)
		if len(silChapters) >= 3 {
			chaptersToUse = mergePartsAndChapters(parts, silChapters)
			useChapters = true
			log.Printf("sidecar-import: text book using v2 silence-based chapters (%d entries, %d parts)",
				len(chaptersToUse), len(parts))
		}
	}

	if !useChapters {
		paused := detectChaptersFromPauses(sc.Words)
		if len(paused) >= 3 && len(paused) <= 80 {
			chaptersToUse = paused
			useChapters = true
			log.Printf("sidecar-import: text book using pause-based chapters (%d)", len(paused))
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
			// Advance the chapter's start past the narrator's title
			// announcement so the body text doesn't lead with
			// "Chapter 21 The Lost Days I awaken from my blackout...".
			// Parts (section headers) have no body so leave them alone.
			startWord := ch.WordIdx
			startSec := ch.Start
			if ch.Src != "part" {
				skip := titleAnnouncementLength(sc.Words, sc.Silences, startWord)
				if skip > 0 && startWord+skip < len(sc.Words) {
					startWord += skip
					startSec = sc.Words[startWord].Start
				}
			}
			ranges = append(ranges, sttChapter{
				Title:     title,
				Start:     startSec,
				End:       end,
				WordIdx:   startWord,
				WordCount: ch.WordCount,
				Src:       ch.Src, // propagate "part" tag through the pipeline
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

	// When v2, pass the silences through to the content builder so
	// paragraph breaks come from acoustic ground truth rather than word-gap
	// math.
	var contentSilences []sttSilence
	if sc.isV2() {
		contentSilences = sc.Silences
	}

	for idx, r := range ranges {
		wStart, wEnd := wordIdxRanges[idx].start, wordIdxRanges[idx].end
		content := buildChapterContentByIdxWithSilences(sc.Words, contentSilences, wStart, wEnd)
		wordCount := 0
		if content != "" {
			wordCount = len(strings.Fields(content))
		}
		// Tag parts explicitly; chapters keep the "transcript" tag so
		// consumers know their content was derived from STT.
		src := "transcript"
		if r.Src == "part" {
			src = "part"
		}
		store.InsertChapter(db.Chapter{
			BookID:     textBookID,
			Index:      idx,
			Title:      r.Title,
			Src:        src,
			Content:    content,
			WordCount:  wordCount,
			StartSec:   r.Start,
			EndSec:     r.End,
			Confidence: 0.9,
		})

		// Build paragraph rows. v2 uses real silence events (ground truth);
		// v1 falls back to word-gap math. These let the FE render the
		// chapter as real paragraphs and only wrap sync-spans for the
		// active one (bounded DOM cost regardless of chapter size).
		var pRanges []idxRange
		if sc.isV2() {
			pRanges = detectParagraphsFromSilences(sc, wStart, wEnd)
		}
		if len(pRanges) == 0 {
			pRanges = detectParagraphsFromPauses(sc.Words, wStart, wEnd)
		}
		for pi, pr := range pRanges {
			// pr.start/end are chapter-local word indices.
			text := buildChapterContentByIdxWithSilences(sc.Words, contentSilences, wStart+pr.start, wStart+pr.end)
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

// mergePartsAndChapters interleaves the two entity types by time and
// tags each with Src="part" or Src="chapter" so downstream code (and the
// UI) can render parts as section headers above their child chapters.
// Dedups near-identical timestamps — if a part and a chapter both start
// within 5 seconds of each other, the part wins (because a real "Part N:
// Chapter 1" announcement is typically read together, and the chapter
// under that part reads as a nested entry right after).
func mergePartsAndChapters(parts, chapters []sttChapter) []sttChapter {
	if len(parts) == 0 {
		return chapters
	}
	for i := range parts {
		parts[i].Src = "part"
	}
	for i := range chapters {
		if chapters[i].Src == "" {
			chapters[i].Src = "chapter"
		}
	}
	out := make([]sttChapter, 0, len(parts)+len(chapters))
	out = append(out, parts...)
	out = append(out, chapters...)
	// Sort by start time.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start < out[j-1].Start; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	// Dedup chapters that sit right at or after a part (within 5s) —
	// the "Part 2. Chapter 6" pattern produces a chapter and a part at
	// essentially the same moment, and we prefer the part as the header.
	const dedupWindow = 5.0
	filtered := out[:0]
	for i, e := range out {
		if e.Src == "chapter" && i > 0 &&
			out[i-1].Src == "part" &&
			e.Start-out[i-1].Start < dedupWindow {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// detectPartsFromSilences finds "Part N" narrator announcements that fall
// immediately after a chapter-grade silence event. This distinguishes real
// Part boundaries ("Part Two. Why Should You Sleep?" after a 4-second break)
// from spurious mid-text references ("in part 2 we'll discuss…") and from
// a narrator reading the TOC aloud (multiple "part N"s packed together).
//
// Returns entries tagged with Src="part" so the UI can render them as
// section headers above their child chapters.
func detectPartsFromSilences(sc *sttSidecar) []sttChapter {
	if !sc.isV2() || len(sc.Silences) == 0 {
		return nil
	}

	// Collect chapter-grade silence end times for time-based proximity match.
	// Index-based matching is fragile because Whisper's word-start timestamp
	// can fall slightly inside silencedetect's silence window at the edges.
	var chapterSilenceEnds []float64
	for _, sil := range sc.Silences {
		if sil.Kind == "chapter" {
			chapterSilenceEnds = append(chapterSilenceEnds, sil.End)
		}
	}
	nearChapterSilence := func(wordStart float64) bool {
		const tolerance = 1.5 // seconds either side of the silence edge
		for _, se := range chapterSilenceEnds {
			if wordStart >= se-tolerance && wordStart <= se+tolerance {
				return true
			}
		}
		return false
	}

	syncWords := sttToSyncTimestamps(sc.Words)
	norm := make([]string, len(syncWords))
	for j := range syncWords {
		norm[j] = normalizeWord(syncWords[j].Word)
	}

	// Scan for "part N" candidates near silence-confirmed positions.
	var parts []sttChapter
	seenNumbers := make(map[int]bool)
	for i := 0; i < len(syncWords)-1; i++ {
		if norm[i] != "part" {
			continue
		}
		if !nearChapterSilence(syncWords[i].Start) {
			continue
		}
		num := parseNumberAt(norm, i+1)
		if num <= 0 || seenNumbers[num] {
			continue
		}
		seenNumbers[num] = true
		parts = append(parts, sttChapter{
			Title:   inferChapterTitleWithSilences(sc.Words, sc.Silences, i, num),
			Start:   syncWords[i].Start,
			WordIdx: i,
		})
	}
	return parts
}

// detectChaptersFromSilences (v2 path) uses real acoustic silence events
// as the primary signal for chapter boundaries. A silence with kind="chapter"
// is a candidate; we then look at the next few words to extract a title
// and check for a narrator announcement ("Chapter N"). This is strictly
// better than v1 pause-based detection because:
//
//   - Silences are measured independently of Whisper's interpolated word
//     timestamps, so we see real pauses that Whisper missed.
//   - Silence duration is ground truth — no more guessing whether a gap
//     is a segment boundary or interpolated zero.
//   - Every silence carries a source tag (silencedetect/vad/both) so we
//     could eventually weight by detector agreement.
func detectChaptersFromSilences(sc *sttSidecar) []sttChapter {
	if !sc.isV2() || len(sc.Silences) == 0 {
		return nil
	}

	// Find all "chapter"-grade silences. The first chapter starts at word 0
	// regardless (books rarely open with 3s of dead air — we want the
	// opening content, not silence).
	var chapters []sttChapter
	chapters = append(chapters, sttChapter{
		Title:   inferChapterTitleWithSilences(sc.Words, sc.Silences, 0, 1),
		Start:   0,
		WordIdx: 0,
	})

	for _, sil := range sc.Silences {
		if sil.Kind != "chapter" {
			continue
		}
		// First word after this silence is where the chapter starts.
		wordIdx := wordAtOrAfter(sc.Words, sil.End)
		if wordIdx < 0 || wordIdx >= len(sc.Words) {
			continue
		}
		// Dedup: don't double-register chapters sharing the same word index
		// (can happen when multiple chapter-silences are adjacent without
		// speech between them, e.g. file-boundary joins).
		if len(chapters) > 0 && chapters[len(chapters)-1].WordIdx == wordIdx {
			continue
		}
		n := len(chapters) + 1
		chapters = append(chapters, sttChapter{
			Title:   inferChapterTitleWithSilences(sc.Words, sc.Silences, wordIdx, n),
			Start:   sc.Words[wordIdx].Start,
			WordIdx: wordIdx,
		})
	}
	return chapters
}

// detectParagraphsFromSilences (v2 path) reads paragraph-grade silence
// events directly and returns idxRange entries for each text chunk between
// consecutive paragraph silences. Much cleaner than v1's word-gap math,
// and uses acoustic ground truth rather than Whisper interpolation.
//
// A silence ONLY breaks paragraphs when the word before it ends with a
// sentence-terminating punctuation mark. Narrators pause mid-title and
// mid-sentence for emphasis, and Whisper drift can place a real
// title-to-body silence inside the title's reported word range. Gating on
// punctuation kills false breaks like "Chapter 21 The | Lost Days I |
// awaken from my blackout" — the words "The" and "I" don't end on .!?.
func detectParagraphsFromSilences(sc *sttSidecar, chapStart, chapEnd int) []idxRange {
	if !sc.isV2() || chapEnd <= chapStart {
		return nil
	}
	// Collect paragraph-grade silence word-indices that fall within this
	// chapter's word range. A silence boundary between word i and i+1
	// means a paragraph ends at i+1.
	boundaries := []int{chapStart}
	for _, sil := range sc.Silences {
		if sil.Kind != "paragraph" && sil.Kind != "chapter" {
			continue
		}
		// Use sil.Start (not End) to find the speaking word: Whisper often
		// drifts the next word's Start into the silence's End range
		// ("hangover." [silence] " As" where " As" Start sits inside the
		// silence), so wordAtOrAfter(End) would skip past the real
		// preceding word and check the wrong punctuation. Mirror the
		// content builder: locate j where words[j+1].Start >= sil.Start.
		wi := -1
		for j := chapStart; j < chapEnd-1; j++ {
			if sc.Words[j+1].Start >= sil.Start {
				if sc.Words[j].Start < sil.Start && endsAtSentence(sc.Words[j].Word) {
					wi = j + 1
				}
				break
			}
		}
		if wi <= chapStart || wi >= chapEnd {
			continue
		}
		if len(boundaries) == 0 || wi > boundaries[len(boundaries)-1] {
			boundaries = append(boundaries, wi)
		}
	}
	boundaries = append(boundaries, chapEnd)

	var out []idxRange
	for i := 0; i+1 < len(boundaries); i++ {
		// Chapter-local indices (caller expects 0-based relative to chapStart).
		out = append(out, idxRange{
			start: boundaries[i] - chapStart,
			end:   boundaries[i+1] - chapStart,
		})
	}
	return out
}

// endsAtSentence reports whether `word` ends with sentence-terminating
// punctuation — the signal that a paragraph break following it is safe.
// Handles trailing close-quotes/parens too: "Hello." → true, "Hello.'" →
// true. Used by paragraph detection to suppress breaks that would land
// mid-sentence (narrator pauses, Whisper drift inside chapter titles).
func endsAtSentence(word string) bool {
	w := strings.TrimSpace(word)
	if w == "" {
		return false
	}
	// Strip trailing close-quote / close-paren characters so "Hello.'"
	// and "Hello.)" still count as sentence-end on the period inside.
	w = strings.TrimRight(w, "\"')”’")
	if w == "" {
		return false
	}
	last := w[len(w)-1]
	return last == '.' || last == '!' || last == '?'
}

// wordAtOrAfter returns the index of the first word whose start time is
// >= target. Returns len(words) if no such word exists. Linear scan is fine
// here — called per silence during import, not on a hot path.
func wordAtOrAfter(words []sttWord, target float64) int {
	for i := range words {
		if words[i].Start >= target {
			return i
		}
	}
	return len(words)
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

// silenceBetween returns the duration of the longest acoustic silence
// event whose start falls in (after, before). Returns 0 if none. Used as
// a fallback signal when Whisper's word-boundary timestamps absorb a real
// silence into adjacent word durations (common — Whisper stretches words
// to fill gaps within a recognized phrase).
func silenceBetween(silences []sttSilence, after, before float64) float64 {
	var best float64
	for _, s := range silences {
		if s.Start > after && s.Start < before {
			if s.Duration > best {
				best = s.Duration
			}
		}
	}
	return best
}

// gapBetweenWords returns the effective acoustic gap between two
// consecutive words. Takes the max of (a) Whisper's word-boundary gap and
// (b) the longest independent silence event observed at the transition.
//
// The "transition" window is widened on the right past b.Start, all the
// way to b.End. Reason: Whisper sometimes drifts the first body word
// backwards into the preceding silence. e.g. Norm Macdonald ch 21,
// "...The Lost Days. [2.2s silence] I awaken from my blackout..." comes
// out of Whisper as words[Days].End=12521.12, words[I].Start=12521.12
// (0s gap), but a real ffmpeg silencedetect event runs 12521.58→12523.78
// — its start (12521.58) sits INSIDE Whisper's reported "I" word
// (12521.12→12522.04). Without this widening the silence is invisible to
// the title-extraction loop and "I" gets glued onto the title.
func gapBetweenWords(words []sttWord, silences []sttSilence, idxA, idxB int) float64 {
	if idxA < 0 || idxB >= len(words) || idxA >= idxB {
		return 0
	}
	a, b := words[idxA], words[idxB]
	whisper := b.Start - a.End
	if whisper < 0 {
		whisper = 0
	}
	const leftSlack = 0.15
	sil := silenceBetween(silences, a.End-leftSlack, b.End)
	if sil > whisper {
		return sil
	}
	return whisper
}

// extractChapterTitle handles the "Chapter/Part/Book N [subtitle]" case.
// Real narrator patterns and how we disambiguate them:
//
//   A. "Chapter 4. [0.6s] Ape Beds, Dinosaurs, and Napping with Half a
//      Brain. [pause] They do not…"
//      → period on number, pause after, subtitle follows. Cut at next
//        period-terminated word.
//
//   B. "Chapter 2 [0.96s] Caffeine, Jet Lag and Melatonin Losing and
//      Gaining Control of Your Sleep Rhythm [pause] …"
//      → no period on number but clear pause after, subtitle follows.
//        Cut at next period OR at a pause >= 0.6s.
//
//   C. "Chapter 1 [0s] What's two plus two? [1.76s] Something about…"
//      → no pause after number, narrator flowed into body. No subtitle.
//
//   D. (Norm Macdonald) "Chapter 23. [pause] Make a Wish [audible 0.7s
//      silence in audio, but Whisper records 0s gap] Atom..."
//      → ground-truth silence comes from the sidecar Silences[] stream,
//        not from word.End → next.Start. We consult both and take the max.
//
// So the real signal is: does the narrator PAUSE after "Chapter N"? That
// pause is the marker of a subtitle announcement. A period helps but
// isn't required (Whisper is inconsistent about inserting them).
func extractChapterTitle(words []sttWord, silences []sttSilence, rawTokens, displayTokens []string, startWord int) string {
	if len(rawTokens) < 2 {
		return strings.Join(rawTokens, " ")
	}

	// Base: "Chapter N" with cleaned number (strip trailing ".,!?:;").
	numberTok := rawTokens[1]
	cleanNum := strings.TrimRight(numberTok, ".!?,;:")
	base := strings.TrimSpace(rawTokens[0]) + " " + cleanNum
	base = strings.ToUpper(base[:1]) + base[1:]

	// Decide whether a subtitle follows. Use the real acoustic gap between
	// the chapter-number word and the next word.
	const ANNOUNCEMENT_PAUSE_MIN = 0.5
	// 0.6s matches TITLE_PAUSE_SECS — a paragraph-break-grade silence.
	// Was 1.0s but real narrator pauses between a short title and the
	// body are typically 0.6-0.8s (e.g. "Make a Wish [0.7s] Atom..."
	// from Norm Macdonald's _Based on a True Story_). Multi-clause
	// titles with comma pauses ("Caffeine, Jet Lag, and Melatonin")
	// still hold together because within-clause gaps are 0.2-0.3s.
	const TITLE_END_PAUSE = 0.6

	_ = ANNOUNCEMENT_PAUSE_MIN // kept for documentation; gating retired below.

	// New gating logic: extract a subtitle only if we find a *title-end*
	// signal — either a sentence-terminator (.!?) on a token or a
	// silence-aware gap >= TITLE_END_PAUSE — within the peek window.
	//
	// The previous gate required a pause IMMEDIATELY after the chapter
	// number ("Chapter 5 [pause] Eight Years Old…"). That broke for
	// narrators (Norm Macdonald) who flow tight from number into title
	// and pause only before the body: "Chapter 4, Six Years Old to Eight
	// Years Old, [1.4s] One day…". Either signal is fine — the absence of
	// EITHER means we don't fabricate a subtitle from possibly-body words.
	var subTokens []string
	foundTitleEnd := false
	for j := 2; j < len(rawTokens); j++ {
		subTokens = append(subTokens, displayTokens[j])
		tok := rawTokens[j]
		if len(tok) > 0 {
			last := tok[len(tok)-1:]
			if last == "." || last == "!" || last == "?" {
				foundTitleEnd = true
				break
			}
		}
		wi := startWord + j
		if wi+1 < len(words) {
			gap := gapBetweenWords(words, silences, wi, wi+1)
			if gap >= TITLE_END_PAUSE {
				foundTitleEnd = true
				break
			}
		}
	}
	if !foundTitleEnd {
		// No clear title boundary — narrator may have flowed straight
		// into body, OR our peek window cut off mid-title. Don't guess.
		return base
	}

	subtitle := strings.TrimSpace(strings.Join(subTokens, ""))
	subtitle = strings.TrimRight(subtitle, ".!?,;: ")
	if subtitle == "" {
		return base
	}
	// If the loop terminated on a silence (not a punctuation mark) and the
	// last word is a short body-starter ("The", "It", "Kramer was..."), it
	// almost certainly bled in from the first sentence of the chapter body.
	// Drop it before casing so we don't lock weird artifacts into the TOC.
	if !subtitleEndsOnPunctuation(subTokens) {
		subtitle = trimTrailingBleedIn(subtitle)
	}
	subtitle = normalizeTitleCase(subtitle)
	full := base + ": " + subtitle
	if len(full) > 80 {
		full = full[:80] + "…"
	}
	return full
}

// bleedInStarters are short tokens that, when they appear as the LAST word of
// a title extracted by silence (not punctuation), almost always belong to the
// first sentence of the chapter body. They're either pronouns or determiners
// that start a sentence — chapter titles rarely END on these words.
var bleedInStarters = map[string]bool{
	"the": true, "a": true, "an": true,
	"it": true, "he": true, "she": true, "they": true, "we": true, "you": true,
	"his": true, "her": true, "their": true, "there": true, "this": true,
	"that": true, "these": true, "those": true,
	"then": true, "and": true, "but": true, "or": true, "so": true, "as": true,
	"in": true, "on": true, "at": true, "to": true, "by": true, "for": true,
	"of": true, "with": true, "from": true, "now": true,
}

// trimTrailingBleedIn removes a single trailing word from a candidate
// subtitle if it looks like a sentence-starter from the chapter body.
// Conservative: only one trailing token, only from a short allowlist of
// common pronouns/determiners/conjunctions — so legitimate titles like
// "A Study in Scarlet" survive (those have the word in the middle, not at
// the end). We only trim if the result is still a non-empty title.
func trimTrailingBleedIn(subtitle string) string {
	fields := strings.Fields(subtitle)
	if len(fields) < 2 {
		return subtitle
	}
	last := strings.ToLower(strings.TrimRight(fields[len(fields)-1], ".,!?:;"))
	if !bleedInStarters[last] {
		return subtitle
	}
	return strings.TrimSpace(strings.Join(fields[:len(fields)-1], " "))
}

// subtitleEndsOnPunctuation reports whether the title-end signal was a
// sentence-terminating punctuation mark on the last token. Used to decide
// whether the last word might be bled-in body content.
func subtitleEndsOnPunctuation(subTokens []string) bool {
	if len(subTokens) == 0 {
		return false
	}
	last := strings.TrimSpace(subTokens[len(subTokens)-1])
	if last == "" {
		return false
	}
	ch := last[len(last)-1]
	return ch == '.' || ch == '!' || ch == '?'
}

// normalizeTitleCase smooths over case-inconsistencies in titles transcribed
// from emphatic narration. Whisper preserves whatever it heard, so a narrator
// pronouncing "CHANGES IN SLEEP ACROSS THE LIFESPAN" with emphasis comes back
// in all caps. We re-case it as Title Case unless it's clearly already mixed
// case (which means it's a real proper-noun title we shouldn't touch).
func normalizeTitleCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	// Count case letter ratios. "Intentionally mixed case" (proper nouns,
	// acronyms) leaves us alone, but a string that's >70% uppercase is
	// emphatic narration ("CHAPTER SEVEN: CATCHING THE FISH The") where a
	// single bled-in or function word makes it look mixed — recase those.
	upper, lower := 0, 0
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			upper++
		} else if r >= 'a' && r <= 'z' {
			lower++
		}
	}
	if upper > 0 && lower > 0 {
		total := upper + lower
		// Only bail when truly mixed — keep "iPhone", "DNA", "Title Case".
		if upper*10 < total*7 {
			return s
		}
		// >=70% uppercase falls through to recasing.
	}
	// All same case — re-case as Title Case. Small connecting words stay
	// lowercase in the middle but the first/last word always capitalizes.
	smallWords := map[string]bool{
		"a": true, "an": true, "and": true, "as": true, "at": true,
		"but": true, "by": true, "for": true, "in": true, "of": true,
		"on": true, "or": true, "the": true, "to": true, "with": true,
	}
	parts := strings.Fields(s)
	for i, p := range parts {
		lower := strings.ToLower(p)
		if i > 0 && i < len(parts)-1 && smallWords[lower] {
			parts[i] = lower
			continue
		}
		if len(p) > 0 {
			parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
		}
	}
	return strings.Join(parts, " ")
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

// titleAnnouncementLength returns the number of words at `start` that
// constitute the narrator's title announcement ("Chapter 21. The Lost
// Days." / "Prologue."), or 0 when no announcement pattern is found.
//
// Mirrors the title-end logic in extractChapterTitle: walks tokens
// starting at start, breaking on the first sentence terminator (.!?)
// OR a silence-aware gap >= TITLE_END_PAUSE. Returns the count of words
// consumed (start..start+count-1 = title region; start+count = body
// start). Caller can then narrow the chapter's word range so the body
// text excludes the spoken title.
//
// Returns 0 for chapters that don't begin with a recognized section
// prefix — the content range stays as-is for "Prelude" (synthetic) and
// for chapters where narrator-pattern detection didn't pick up a real
// announcement.
func titleAnnouncementLength(words []sttWord, silences []sttSilence, start int) int {
	const peek = 20
	if start < 0 || start >= len(words) {
		return 0
	}
	end := start + peek
	if end > len(words) {
		end = len(words)
	}
	if start+1 >= end {
		return 0
	}
	first := strings.ToLower(strings.TrimRight(strings.TrimSpace(words[start].Word), ".,!?:;"))
	// Single-word section markers ("Prologue", "Foreword", ...): the
	// announcement is just that one word. Body starts at start+1.
	for _, p := range sectionPrefixes {
		if first == p && p != "chapter" && p != "part" && p != "book" {
			return 1
		}
	}
	// "Chapter|Part|Book N [subtitle]" — body starts after the last title
	// word. Walk forward until we hit a sentence terminator OR a real
	// pause (matching TITLE_END_PAUSE = 0.6s used by extractChapterTitle).
	isCh := false
	for _, p := range []string{"chapter", "part", "book"} {
		if first == p {
			isCh = true
			break
		}
	}
	if !isCh {
		return 0
	}
	const titleEndPause = 0.6
	for j := start + 2; j < end; j++ {
		tok := strings.TrimSpace(words[j].Word)
		if tok != "" {
			last := tok[len(tok)-1]
			if last == '.' || last == '!' || last == '?' {
				return j - start + 1
			}
		}
		if j+1 < end {
			gap := gapBetweenWords(words, silences, j, j+1)
			if gap >= titleEndPause {
				return j - start + 1
			}
		}
	}
	// No reliable title-end signal in the peek window — narrator may
	// have run straight into body. Trim only the prefix + number so we
	// at least drop "Chapter 21".
	return 2
}

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
	return inferChapterTitleWithSilences(words, nil, start, chapNum)
}

// inferChapterTitleWithSilences extends inferChapterTitle with the v3
// independent silence event stream. When silences is non-nil, real
// acoustic pauses (which Whisper sometimes records as 0s word gaps) are
// used as the title-boundary signal — fixing cases like Norm Macdonald's
// "Make a Wish [audible silence] Atom" where word.End→next.Start is 0.
func inferChapterTitleWithSilences(words []sttWord, silences []sttSilence, start, chapNum int) string {
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
			gap := gapBetweenWords(words, silences, i, i+1)
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
	//
	// Two-token rule: "Chapter 4. Ape Beds, Dinosaurs..." needs us to
	// *skip* the period attached to "4." (it's the announcement period,
	// not the subtitle-end period). Then we look for the REAL title end:
	// either a sentence-ending period followed by capital letter, OR a
	// significant pause.
	//
	// If there's no period after the number at all ("Chapter 1 What's..."),
	// the narrator went straight into body — no subtitle exists, so we
	// just return "Chapter N".
	for _, prefix := range []string{"chapter", "part", "book"} {
		if strings.HasPrefix(lower, prefix+" ") {
			title := extractChapterTitle(words, silences, rawTokens, displayTokens, start)
			return NormalizeChapterTitle(strings.TrimSpace(title))
		}
	}

	// Case 2: single-word section marker.
	firstLower := strings.ToLower(rawTokens[0])
	for _, p := range sectionPrefixes {
		if firstLower == p {
			return strings.ToUpper(p[:1]) + p[1:]
		}
	}

	// Case 3: no clean chapter announcement. Don't fabricate a title from a
	// content snippet — that produces inconsistent TOCs ("Ch 1 · This is
	// Audible" alongside "Chapter 2: Real Title"). Just return "Chapter N".
	//
	// Exception: if the first 1-2 tokens are a bare chapter number ("Two.",
	// "5.") the v2 silence-detection path produced this, and a real subtitle
	// often follows. Promote those to "Chapter N: Subtitle".
	skipLeadingNumberTokens := 0
	for i := 0; i < len(rawTokens) && i < 2; i++ {
		t := strings.TrimRight(strings.ToLower(rawTokens[i]), ".,!?:;")
		if isNumberWord(t) || isAllDigits(t) {
			skipLeadingNumberTokens = i + 1
		} else {
			break
		}
	}
	if skipLeadingNumberTokens > 0 {
		effective := displayTokens[skipLeadingNumberTokens:]
		if len(effective) > 8 {
			effective = effective[:8]
		}
		text := strings.TrimSpace(strings.Join(effective, ""))
		text = strings.TrimRight(text, ".,;: ")
		if text != "" {
			text = normalizeTitleCase(text)
			full := fmt.Sprintf("Chapter %d: %s", chapNum, text)
			if len(full) > 80 {
				full = full[:80] + "…"
			}
			return NormalizeChapterTitle(full)
		}
	}
	return fmt.Sprintf("Chapter %d", chapNum)
}

// isNumberWord returns true for "one", "two", ..., "twenty" plus "first",
// "second", "third" style ordinals. Used to recognize chapter-number words
// the narrator may say at the start of a pause-detected chapter boundary.
func isNumberWord(s string) bool {
	switch s {
	case "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen",
		"eighteen", "nineteen", "twenty", "twentyone", "thirty",
		"first", "second", "third", "fourth", "fifth", "sixth", "seventh",
		"eighth", "ninth", "tenth":
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
		if gap >= PARAGRAPH_PAUSE_SECS && endsAtSentence(words[i].Word) {
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

// fillNumberingGaps walks the detected run looking for gaps in chapter
// numbering (e.g. 13 → 16 means 14 and 15 are missing) and inserts inferred
// chapter entries inside the gap so the user-facing TOC is continuous.
//
// Inference strategy:
//   - For each missing number, locate a chapter-grade silence (kind="chapter")
//     in the time window between the surrounding detected chapters, biased
//     toward the expected proportional position.
//   - If no usable silence exists, fall back to evenly partitioning the
//     gap by missing-count so we still emit something better than a hole.
//
// The inferred chapter title is "Chapter N" (no subtitle) since we don't
// have a narrator announcement to extract one from.
func fillNumberingGaps(detected []DetectedChapter, sc *sttSidecar) []DetectedChapter {
	if len(detected) < 2 {
		return detected
	}

	// Skip if the run is "Part" — we don't have part-grade silences to fill.
	if detected[0].Kind != "chapter" {
		return detected
	}

	// Collect chapter-grade silence times so we can pick boundaries from
	// real acoustic events. v1 sidecars have none — we'll fall back to even
	// partitioning.
	var silenceTimes []float64
	if sc.isV2() {
		for _, sil := range sc.Silences {
			if sil.Kind == "chapter" {
				silenceTimes = append(silenceTimes, sil.End)
			}
		}
	}

	out := make([]DetectedChapter, 0, len(detected))
	for i := 0; i < len(detected); i++ {
		out = append(out, detected[i])
		if i+1 >= len(detected) {
			continue
		}
		curr := detected[i]
		next := detected[i+1]
		missing := next.Number - curr.Number - 1
		if missing <= 0 {
			continue
		}
		gapStart := curr.StartSec
		gapEnd := next.StartSec
		boundaries := chooseGapBoundaries(silenceTimes, gapStart, gapEnd, missing)
		for k := 1; k <= missing; k++ {
			number := curr.Number + k
			startSec := boundaries[k-1]
			out = append(out, DetectedChapter{
				Number:     number,
				Kind:       "chapter",
				Title:      titleFor("chapter", number),
				StartSec:   startSec,
				WordIdx:    wordAtOrAfter(sc.Words, startSec),
				Confidence: 0.4, // inferred — lower than detected (0.5+ base)
			})
		}
		log.Printf("sidecar-import: filled %d-chapter gap between Chapter %d and Chapter %d (silences=%d)",
			missing, curr.Number, next.Number, len(silenceTimes))
	}

	// Recompute Index + EndSec on the merged slice.
	for i := range out {
		out[i].Index = i
		if i+1 < len(out) {
			out[i].EndSec = out[i+1].StartSec
		} else if sc.Duration > 0 {
			out[i].EndSec = sc.Duration
		}
	}
	return out
}

// chooseGapBoundaries picks `missing` chapter boundaries inside the gap
// (gapStart, gapEnd), preferring chapter-grade silences when available and
// falling back to even time-partitioning when not. The returned slice is
// strictly time-ordered so inferred chapters never overlap or invert.
//
// Algorithm: collect candidate silences inside the gap, sort by time. For
// each of the `missing` expected proportional positions, walk forward
// picking the silence closest to that position that is also AFTER the
// previously picked silence. If we run out of silences (or there were
// none), fall back to even time partitioning for the remaining slots.
func chooseGapBoundaries(allSilences []float64, gapStart, gapEnd float64, missing int) []float64 {
	out := make([]float64, missing)

	// Collect + sort silences strictly inside the gap.
	var gapSil []float64
	for _, t := range allSilences {
		if t > gapStart && t < gapEnd {
			gapSil = append(gapSil, t)
		}
	}
	sort.Float64s(gapSil)

	prev := gapStart
	silIdx := 0
	for k := 0; k < missing; k++ {
		expected := gapStart + float64(k+1)/float64(missing+1)*(gapEnd-gapStart)

		// Among silences strictly after `prev`, pick the one closest to
		// `expected`. Walk forward from silIdx; once we pass `expected`
		// we know subsequent silences can only get farther, so we can
		// stop after seeing one bracket.
		bestIdx := -1
		bestDist := -1.0
		for i := silIdx; i < len(gapSil); i++ {
			if gapSil[i] <= prev {
				continue
			}
			d := gapSil[i] - expected
			if d < 0 {
				d = -d
			}
			if bestIdx == -1 || d < bestDist {
				bestIdx = i
				bestDist = d
				continue
			}
			// Past expected and getting worse — done.
			if gapSil[i] > expected {
				break
			}
		}

		if bestIdx == -1 {
			// No usable silence remaining — fall back to even partition.
			out[k] = expected
		} else {
			out[k] = gapSil[bestIdx]
			prev = gapSil[bestIdx]
			silIdx = bestIdx + 1
		}

		// Guarantee strictly-increasing time even in pathological inputs.
		if k > 0 && out[k] <= out[k-1] {
			out[k] = out[k-1] + 1.0
		}
	}
	return out
}

// narratorRunIsStrong reports whether a DetectChapters result is good enough
// to override silence-based detection. We require both a meaningful count
// (>=5) and reasonable book coverage (last chapter starts past 60% of the
// book) — without coverage, the run might be missing the entire first half
// (e.g., narrator-pattern only detected chapters 16-31). Short books (<1h)
// pass with just the count check since coverage math is noisy.
func narratorRunIsStrong(detected []DetectedChapter, durationSec float64) bool {
	if len(detected) < 5 {
		return false
	}
	if durationSec < 3600 {
		return true
	}
	last := detected[len(detected)-1].StartSec
	return last >= durationSec*0.60
}

// detectedToSttChapters converts DetectChapters results into the sttChapter
// shape used by sidecar_import, re-running inferChapterTitle so we pick up
// any subtitle that follows the "Chapter N" announcement. Silences feed
// the title-end detection so acoustic pauses Whisper missed still cut
// the title cleanly.
func detectedToSttChapters(words []sttWord, silences []sttSilence, detected []DetectedChapter) []sttChapter {
	out := make([]sttChapter, len(detected))
	for i, d := range detected {
		title := inferChapterTitleWithSilences(words, silences, d.WordIdx, d.Number)
		if title == "" {
			title = d.Title
		}
		out[i] = sttChapter{
			Title:   title,
			Start:   d.StartSec,
			End:     d.EndSec,
			WordIdx: d.WordIdx,
		}
	}
	return out
}

// sttToSyncTimestamps converts sidecar word records into the internal
// SyncTimestamp type used by DetectChapters. Field names match, just a
// struct copy.
func sttToSyncTimestamps(ws []sttWord) []db.SyncTimestamp {
	out := make([]db.SyncTimestamp, len(ws))
	for i, w := range ws {
		out[i] = db.SyncTimestamp{Start: w.Start, End: w.End, Word: w.Word}
	}
	return out
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
	return buildChapterContentByIdxWithSilences(words, nil, start, end)
}

// buildChapterContentByIdxWithSilences is the v2-aware content builder.
// When `silences` is non-nil, paragraph breaks come from real acoustic
// silence events (kind="paragraph" or larger) rather than word-gap math.
// Word gaps are unreliable for v1 sidecars (Whisper interpolates
// within-segment timestamps), so when we have silences we prefer them.
func buildChapterContentByIdxWithSilences(words []sttWord, silences []sttSilence, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(words) {
		end = len(words)
	}
	if start >= end {
		return ""
	}

	// If we have silences, compute a set of word-indices (within the chapter)
	// AFTER which a paragraph break should appear.
	var paraBreakAfter map[int]bool
	if len(silences) > 0 {
		paraBreakAfter = make(map[int]bool)
		for _, sil := range silences {
			if sil.Kind != "paragraph" && sil.Kind != "chapter" {
				continue
			}
			// The silence happens between word i and word i+1. Find the
			// last word whose Start < silence.Start.
			for j := start; j < end-1; j++ {
				if words[j+1].Start >= sil.Start {
					if words[j].Start < sil.Start && endsAtSentence(words[j].Word) {
						paraBreakAfter[j] = true
					}
					break
				}
			}
		}
	}

	var b strings.Builder
	startingParagraph := true
	for i := start; i < end; i++ {
		word := words[i].Word
		// If we just broke to a new paragraph, strip the leading space that
		// Whisper attaches to each word ("hello", " world" format) so the
		// paragraph starts cleanly.
		if startingParagraph {
			word = strings.TrimLeft(word, " ")
			startingParagraph = false
		}
		b.WriteString(word)
		if i+1 >= end {
			continue
		}
		insertBreak := false
		if paraBreakAfter != nil {
			insertBreak = paraBreakAfter[i]
		} else {
			// v1 fallback: word-gap math. Same sentence-end guard.
			gap := words[i+1].Start - words[i].End
			insertBreak = gap >= PARAGRAPH_PAUSE_SECS && endsAtSentence(words[i].Word)
		}
		if insertBreak {
			b.WriteString("\n\n")
			startingParagraph = true
		}
	}
	return strings.TrimSpace(b.String())
}
