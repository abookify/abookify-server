// Chapter detection from word-level STT timestamps.
//
// Works on the transcript alone — no ebook required. The primary signal is the
// narrator speaking a chapter convention ("Chapter one", "Part three", etc.).
// We filter false positives by requiring the numbers form a monotonic run, and
// boost confidence when a candidate is adjacent to a silence gap in the audio.
package library

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Thin wrapper so we don't import encoding/json in many places.
func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// DetectedChapter is a single chapter boundary discovered in a transcript.
type DetectedChapter struct {
	Index      int     // 0-based order within the audio book
	Number     int     // Parsed chapter number (1, 2, ...)
	Kind       string  // "chapter" or "part"
	Title      string  // "Chapter 1", "Part 3"
	StartSec   float64 // time in the audio file
	EndSec     float64 // start of the next chapter (or file duration)
	WordIdx    int     // index into the word stream where the title begins
	Confidence float64 // 0.0–1.0
	HasSilence bool    // candidate is preceded by a silence gap (genuine announcement signal)
}

// detectionOpts tunes the algorithm. Exposed for tests; callers use defaults.
type detectionOpts struct {
	SilenceGapSecs   float64 // gap (word[i+1].start - word[i].end) that counts as "silence"
	SilenceBoost     float64 // confidence added when a candidate has silence right before it
	SequenceBoost    float64 // confidence added when a candidate is part of a monotonic run
	BaseConfidence   float64 // starting confidence for any pattern match
	SequenceTolerance int    // allowed gap in chapter numbering (1 = allow skipping one)
}

func defaultDetectionOpts() detectionOpts {
	return detectionOpts{
		SilenceGapSecs:    2.0,
		SilenceBoost:      0.2,
		SequenceBoost:     0.3,
		BaseConfidence:    0.5,
		// Allow up to 2 missing chapter announcements in a row — Whisper
		// occasionally drops a chapter cue, and we'd rather bridge a small
		// gap than split a 30-chapter book into two short runs. Gap-bridging
		// candidates must have a silence boost (validateSequence enforces),
		// which prevents orphan dialogue references from filling gaps.
		SequenceTolerance: 2,
	}
}

// DetectChapters scans the word stream for narrator chapter cues and returns
// a validated, ordered list of chapter boundaries. durationSec is the total
// audio length and sets the end of the final chapter.
func DetectChapters(words []db.SyncTimestamp, durationSec float64) []DetectedChapter {
	return detectChaptersWithOpts(words, durationSec, defaultDetectionOpts())
}

func detectChaptersWithOpts(words []db.SyncTimestamp, durationSec float64, opts detectionOpts) []DetectedChapter {
	if len(words) == 0 {
		return nil
	}

	// Normalize once — lowercase, strip punctuation (see text_align.normalizeWord).
	norm := make([]string, len(words))
	for i, w := range words {
		norm[i] = normalizeWord(w.Word)
	}

	// Gather raw candidates for both conventions, then validate separately.
	// A book can contain Parts AND Chapters (e.g. Tolstoy); we emit whichever
	// sequence is longer. Merging both is rarely what the user wants — most
	// readers navigate by the inner unit.
	chapterCands := findCandidates(words, norm, "chapter", opts)
	partCands := findCandidates(words, norm, "part", opts)

	chapterValid := validateSequence(chapterCands, opts)
	partValid := validateSequence(partCands, opts)

	var chosen []DetectedChapter
	kind := "chapter"
	if len(partValid) > len(chapterValid) {
		chosen = partValid
		kind = "part"
	} else {
		chosen = chapterValid
	}

	if len(chosen) == 0 {
		log.Printf("chapter-detect: no chapter sequence found (%d chapter candidates, %d part candidates)",
			len(chapterCands), len(partCands))
		return nil
	}

	// Finalize: index + end times + titles.
	for i := range chosen {
		chosen[i].Index = i
		chosen[i].Kind = kind
		chosen[i].Title = titleFor(kind, chosen[i].Number)
		if i+1 < len(chosen) {
			chosen[i].EndSec = chosen[i+1].StartSec
		} else {
			chosen[i].EndSec = durationSec
		}
	}

	log.Printf("chapter-detect: %d %ss (rejected %d of %d raw matches)",
		len(chosen), kind, len(chapterCands)+len(partCands)-len(chosen),
		len(chapterCands)+len(partCands))
	return chosen
}

// findCandidates emits every spot where `keyword` is followed by a number word
// or digit. Confidence is the base score + silence boost; sequence boost is
// applied later in validateSequence.
func findCandidates(words []db.SyncTimestamp, norm []string, keyword string, opts detectionOpts) []DetectedChapter {
	var out []DetectedChapter
	for i := 0; i < len(norm)-1; i++ {
		if norm[i] != keyword {
			continue
		}
		num := parseNumberAt(norm, i+1)
		if num <= 0 {
			continue
		}
		conf := opts.BaseConfidence
		hasSilence := false
		// Silence boost: was there a significant pause right before this word?
		if i > 0 {
			gap := words[i].Start - words[i-1].End
			if gap >= opts.SilenceGapSecs {
				conf += opts.SilenceBoost
				hasSilence = true
			}
		} else {
			// First word in the stream — treat as preceded by silence
			// (book intro openings always start fresh).
			hasSilence = true
		}
		out = append(out, DetectedChapter{
			Number:     num,
			StartSec:   words[i].Start,
			WordIdx:    i,
			Confidence: conf,
			HasSilence: hasSilence,
		})
	}
	return out
}

// validateSequence walks candidates and keeps only those that form a monotonic
// run. Orphan matches (Chapter 17 with no Chapter 16 nearby) are dropped. The
// winner is the run with the most kept entries.
//
// Algorithm: dynamic programming — for each candidate, find the longest
// sequence ending at that candidate where each next number is last+1 (±tolerance).
// At the end, walk back from the best tail to rebuild the selected run.
func validateSequence(cands []DetectedChapter, opts detectionOpts) []DetectedChapter {
	if len(cands) == 0 {
		return nil
	}
	// best[i] = length of longest run ending at i.
	// prev[i] = index of the previous candidate in that run (-1 if none).
	best := make([]int, len(cands))
	prev := make([]int, len(cands))
	for i := range cands {
		best[i] = 1
		prev[i] = -1
		for j := 0; j < i; j++ {
			// Must be later in time AND the next expected number.
			if cands[j].StartSec >= cands[i].StartSec {
				continue
			}
			diff := cands[i].Number - cands[j].Number
			if diff < 1 || diff > 1+opts.SequenceTolerance {
				continue
			}
			// Gap-bridging extension (diff > 1) requires a real silence
			// before the candidate. Without this guard, an orphan
			// dialogue reference like "Chapter seven" mid-sentence could
			// extend a real run and produce a false boundary. Real
			// chapter announcements always have a silent pause before
			// them; mid-text references don't.
			if diff > 1 && !cands[i].HasSilence {
				continue
			}
			if best[j]+1 > best[i] {
				best[i] = best[j] + 1
				prev[i] = j
			}
		}
	}
	// Find the tail of the best run. Prefer runs that START at a low number
	// (ideally 1) — more likely to be the real chapter sequence rather than a
	// coincidental "chapter seventeen" from dialogue.
	bestEnd := 0
	for i := 1; i < len(cands); i++ {
		if best[i] > best[bestEnd] {
			bestEnd = i
			continue
		}
		if best[i] == best[bestEnd] {
			// Tie-break: prefer the run whose start number is lower (closer to 1).
			startI := firstInRun(cands, prev, i)
			startBest := firstInRun(cands, prev, bestEnd)
			if cands[startI].Number < cands[startBest].Number {
				bestEnd = i
			}
		}
	}

	// Require at least 2 in a row to be confident this is a real sequence.
	// A lone "Chapter one" with nothing after is probably a phrase, not structure.
	if best[bestEnd] < 2 {
		return nil
	}

	// Reconstruct.
	var run []DetectedChapter
	for i := bestEnd; i != -1; i = prev[i] {
		run = append(run, cands[i])
	}
	// Reverse into start-to-end order.
	for i, j := 0, len(run)-1; i < j; i, j = i+1, j-1 {
		run[i], run[j] = run[j], run[i]
	}

	// Apply sequence boost to confirmed members.
	for i := range run {
		run[i].Confidence += opts.SequenceBoost
		if run[i].Confidence > 1.0 {
			run[i].Confidence = 1.0
		}
	}
	return run
}

func firstInRun(cands []DetectedChapter, prev []int, i int) int {
	for prev[i] != -1 {
		i = prev[i]
	}
	return i
}

// parseNumberAt reads a number starting at idx. Handles digits, single number
// words ("seventeen"), and compound words ("twenty" "three"). Returns 0 on miss.
func parseNumberAt(norm []string, idx int) int {
	if idx >= len(norm) {
		return 0
	}
	// Digits first — easiest case.
	if n, err := strconv.Atoi(norm[idx]); err == nil && n > 0 && n < 1000 {
		return n
	}
	// Single-word numbers.
	if n, ok := numberWords[norm[idx]]; ok {
		// Compound: "twenty three" → 23. Only if first word is a tens-multiple.
		if isTens(norm[idx]) && idx+1 < len(norm) {
			if m, ok2 := numberWords[norm[idx+1]]; ok2 && m < 10 {
				return n + m
			}
		}
		return n
	}
	return 0
}

var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
	"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
	"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

func isTens(w string) bool {
	switch w {
	case "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety":
		return true
	}
	return false
}

func titleFor(kind string, num int) string {
	// Capitalize first letter of kind.
	k := strings.ToUpper(kind[:1]) + kind[1:]
	return k + " " + strconv.Itoa(num)
}

// NormalizeChapterTitle unifies the surface form of chapter titles produced
// by different detection paths so the user-facing TOC isn't a mix of:
//
//   "Chapter One", "Chapter 2", "Ch 3", "Ch. 4: Subtitle", "Chapter Forty-two: ..."
//
// All of those collapse to:
//
//   "Chapter 1", "Chapter 2", "Chapter 3", "Chapter 4: Subtitle", "Chapter 42: ..."
//
// Rules:
//   - Prefix unified to "Chapter" / "Part" — accepts "Ch", "Ch.", "Chap",
//     "Chapter", and case variants.
//   - Number portion converted to digits — "One" → "1", "Twenty-Three" → "23".
//     Recognized: number words 1-99 (including hyphenated compounds).
//   - Subtitle (if any, after ":" or " - ") preserved unchanged.
//   - Returns the input untouched if no recognized prefix is found, so
//     non-Chapter titles ("Prologue", "Foreword", etc.) pass through.
func NormalizeChapterTitle(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return t
	}

	// Split off the subtitle, if any. Try ": " first, then " - " as a
	// secondary separator some narrators use.
	prefix, subtitle := t, ""
	for _, sep := range []string{": ", " - ", " — "} {
		if i := strings.Index(t, sep); i > 0 {
			prefix = t[:i]
			subtitle = strings.TrimSpace(t[i+len(sep):])
			break
		}
	}

	// Tokenize the prefix to identify [kind] [number...].
	tokens := strings.Fields(prefix)
	if len(tokens) < 2 {
		return title
	}

	kindRaw := strings.ToLower(strings.TrimRight(tokens[0], ".,!?:;"))
	var kind string
	switch kindRaw {
	case "ch", "chap", "chapter":
		kind = "Chapter"
	case "part":
		kind = "Part"
	case "book":
		kind = "Book"
	default:
		// Not a recognized chapter prefix — leave the title alone.
		return title
	}

	// Parse the number portion, which may be one digit token, one number
	// word, or two tokens for compound number words ("twenty three" or
	// "twenty-three"). Only consume tokens that participate in the number.
	num, consumed := parseTitleNumber(tokens[1:])
	if num <= 0 {
		// Couldn't recognize a number — keep original to avoid false
		// normalization.
		return title
	}

	out := fmt.Sprintf("%s %d", kind, num)

	// If the prefix had extra words after the number (e.g. "Chapter 4 The"
	// where "The" leaked in from the body), append them to the subtitle
	// rather than dropping them — preserves whatever signal was there.
	leftover := strings.Join(tokens[1+consumed:], " ")
	leftover = strings.TrimRight(leftover, ".,!?:; ")

	pieces := []string{}
	if leftover != "" {
		pieces = append(pieces, leftover)
	}
	if subtitle != "" {
		pieces = append(pieces, subtitle)
	}
	if len(pieces) > 0 {
		joined := strings.Join(pieces, " ")
		// Re-trim: subtitle may already have ended with a period etc.
		joined = strings.TrimRight(joined, ".,!?:; ")
		if joined != "" {
			out = out + ": " + joined
		}
	}
	return out
}

// parseTitleNumber pulls a chapter number from the leading tokens. Returns
// the number and how many tokens it consumed. Handles digits, single
// number words, and compound number words ("twenty-three", "thirty four").
func parseTitleNumber(tokens []string) (int, int) {
	if len(tokens) == 0 {
		return 0, 0
	}
	first := strings.ToLower(strings.TrimRight(tokens[0], ".,!?:;"))

	// Digit form.
	if n, err := strconv.Atoi(first); err == nil && n > 0 && n < 1000 {
		return n, 1
	}

	// Hyphenated compound: "twenty-three"
	if strings.Contains(first, "-") {
		parts := strings.SplitN(first, "-", 2)
		if a, ok := numberWords[parts[0]]; ok && isTens(parts[0]) {
			if b, ok := numberWords[parts[1]]; ok && b < 10 {
				return a + b, 1
			}
		}
	}

	// Single word: "one", "twelve", "thirty"
	if n, ok := numberWords[first]; ok {
		// Two-token compound: "twenty three"
		if isTens(first) && len(tokens) > 1 {
			second := strings.ToLower(strings.TrimRight(tokens[1], ".,!?:;"))
			if m, ok2 := numberWords[second]; ok2 && m < 10 {
				return n + m, 2
			}
		}
		return n, 1
	}

	return 0, 0
}

// DetectChaptersFromStoredSync loads existing sync data for a work's single
// audio book, runs detection, splits the transcript to match, and relinks
// audio↔text chapters. Returns the count of detected chapters. Intended for
// re-running detection on already-transcribed books without touching STT.
func DetectChaptersFromStoredSync(store *db.Store, workID int64) (int, error) {
	work, err := store.GetWork(workID)
	if err != nil {
		return 0, err
	}
	if work == nil || len(work.AudioFiles) != 1 {
		return 0, nil
	}
	af := work.AudioFiles[0]
	raw, err := store.GetSyncData(workID, af.ID, 0)
	if err != nil || raw == "" {
		return 0, err
	}
	var words []db.SyncTimestamp
	if err := jsonUnmarshal(raw, &words); err != nil {
		return 0, err
	}
	duration := af.Duration
	if duration == 0 && len(words) > 0 {
		duration = words[len(words)-1].End
	}
	detected := DetectChapters(words, duration)
	if len(detected) == 0 {
		return 0, nil
	}
	writeDetectedChapters(store, af.ID, detected)

	// Split any transcript book on the same boundaries. A transcript is the
	// synthetic text book produced by STT (format=transcript). Real EPUBs are
	// left alone — their structure comes from the source file.
	var transcriptID int64
	for _, tf := range work.TextFiles {
		if tf.Format == "transcript" {
			transcriptID = tf.ID
			break
		}
	}
	if transcriptID != 0 {
		if _, err := SplitTranscriptByChapters(store, transcriptID, words, detected); err != nil {
			log.Printf("split-transcript: %v", err)
		}
	}

	// Relink against any paired ebook (or the now-split transcript) so the
	// audio chapters point at their text counterparts.
	if fresh, ferr := store.GetWork(workID); ferr == nil && fresh != nil {
		if err := LinkChapters(store, fresh); err != nil {
			log.Printf("link-chapters after detect: %v", err)
		}
	}
	return len(detected), nil
}

// writeDetectedChapters replaces the audio book's chapters with the supplied
// detected list. Any existing chapters for the book are deleted first.
func writeDetectedChapters(store *db.Store, audioBookID int64, detected []DetectedChapter) {
	if len(detected) == 0 {
		return
	}
	if err := store.DeleteChaptersByBook(audioBookID); err != nil {
		log.Printf("chapter-detect: clear existing chapters for book %d: %v", audioBookID, err)
		return
	}
	for _, d := range detected {
		ch := db.Chapter{
			BookID:     audioBookID,
			Index:      d.Index,
			Title:      d.Title,
			Src:        "detected",
			StartSec:   d.StartSec,
			EndSec:     d.EndSec,
			Confidence: d.Confidence,
		}
		if err := store.InsertChapter(ch); err != nil {
			log.Printf("chapter-detect: insert chapter %d for book %d: %v", d.Index, audioBookID, err)
		}
	}
	log.Printf("chapter-detect: saved %d chapters for book %d", len(detected), audioBookID)
}
