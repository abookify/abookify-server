package library

import (
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
)

var chapterNumRe = regexp.MustCompile(`(?i)(?:chapter|ch\.?|letter|part)\s*(\d+)`)
var romanRe = regexp.MustCompile(`(?i)(?:chapter|ch\.?|part)\s+((?:x{0,3})(?:ix|iv|v?i{0,3}))$`)

// audioChapter is one unit on the audio side that wants a text-chapter link.
// For multi-file audiobooks, one per file. For single-file books with detected
// chapters, one per detected chapter — both share the same audio_index space
// in chapter_links.
type audioChapter struct {
	bookID int64
	idx    int    // value for chapter_links.audio_index
	title  string // title used for matching (file title, or "Chapter N")
}

// LinkChapters matches audio chapters to text chapters for a work and writes
// chapter_links. Existing links for the work are wiped first — re-running is
// safe and produces a clean set.
//
// The audio side is a flat list of "audio chapters" produced by
// flattenAudioChapters: either one entry per audio file, or one per detected
// chapter inside a single-file audiobook.
func LinkChapters(store *db.Store, work *db.Work) error {
	if !work.HasAudio || !work.HasText {
		return nil
	}

	// Pick the text book to link against — the first one with chapters.
	var textBookID int64
	for _, f := range work.TextFiles {
		if f.ChapterCount > 0 {
			textBookID = f.ID
			break
		}
	}
	if textBookID == 0 {
		return nil
	}

	textChapters, err := store.ListChapters(textBookID)
	if err != nil {
		return err
	}
	if len(textChapters) == 0 {
		return nil
	}

	// Lookup structures for text chapters.
	textByNum := map[int]int{}
	textByNorm := map[string]int{}
	for _, ch := range textChapters {
		textByNorm[normalize(ch.Title)] = ch.Index
		if num := extractChapterNum(ch.Title); num > 0 {
			textByNum[num] = ch.Index
		}
	}

	audioChapters, err := flattenAudioChapters(store, work)
	if err != nil {
		return err
	}
	if len(audioChapters) == 0 {
		return nil
	}

	// Clean slate — old links may reference a different chapter count/layout.
	if err := store.DeleteChapterLinksByWork(work.ID); err != nil {
		return err
	}

	linkCount := 0
	for _, ac := range audioChapters {
		bestTextIdx, confidence := matchTextChapter(ac.title, textByNum, textByNorm, textChapters)
		if bestTextIdx < 0 {
			continue
		}
		if err := store.InsertChapterLink(work.ID, db.ChapterLink{
			AudioBookID: ac.bookID,
			AudioIndex:  ac.idx,
			TextBookID:  textBookID,
			TextIndex:   bestTextIdx,
			Confidence:  confidence,
		}); err != nil {
			return err
		}
		linkCount++
	}

	if linkCount > 0 {
		log.Printf("linked %d/%d audio chapters to text for %q",
			linkCount, len(audioChapters), work.Title)
	}
	return nil
}

// flattenAudioChapters returns the linkable units on the audio side.
// Single-file book with detected chapters → one entry per detected chapter
// (audio_index = detected chapter index). Otherwise → one entry per audio file
// (audio_index = file position within the work).
func flattenAudioChapters(store *db.Store, work *db.Work) ([]audioChapter, error) {
	// Only treat detected chapters specially when there's exactly one audio book.
	// Multi-file works don't run chapter detection (the files already are chapters).
	if len(work.AudioFiles) == 1 {
		af := work.AudioFiles[0]
		detected, err := store.ListChapters(af.ID)
		if err != nil {
			return nil, err
		}
		if len(detected) > 0 {
			out := make([]audioChapter, 0, len(detected))
			for _, ch := range detected {
				out = append(out, audioChapter{
					bookID: af.ID,
					idx:    ch.Index,
					title:  ch.Title,
				})
			}
			return out, nil
		}
	}

	// Fall back: one entry per audio file.
	out := make([]audioChapter, 0, len(work.AudioFiles))
	for i, af := range work.AudioFiles {
		title := af.Title
		if title == "" {
			title = af.Filename
		}
		out = append(out, audioChapter{
			bookID: af.ID,
			idx:    i,
			title:  title,
		})
	}
	return out, nil
}

// matchTextChapter applies the three-strategy match (number, normalized title,
// word overlap) and returns the best text-chapter index or -1.
func matchTextChapter(audioTitle string, textByNum map[int]int, textByNorm map[string]int, textChapters []db.Chapter) (int, float64) {
	// 1. Extracted chapter number.
	if num := extractChapterNum(audioTitle); num > 0 {
		if idx, ok := textByNum[num]; ok {
			return idx, 0.9
		}
	}
	// 2. Normalized title exact-match.
	norm := normalize(audioTitle)
	if idx, ok := textByNorm[norm]; ok {
		return idx, 0.8
	}
	// 3. Word-overlap heuristic — pick best-scoring title with ≥ 2 shared words.
	bestScore := 0
	bestIdx := -1
	for _, ch := range textChapters {
		score := overlapScore(norm, normalize(ch.Title))
		if score > bestScore && score >= 2 {
			bestScore = score
			bestIdx = ch.Index
		}
	}
	if bestIdx < 0 {
		return -1, 0
	}
	conf := float64(bestScore) * 0.2
	if conf > 0.7 {
		conf = 0.7
	}
	return bestIdx, conf
}

func extractChapterNum(title string) int {
	// Try arabic numerals first
	m := chapterNumRe.FindStringSubmatch(title)
	if m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return n
		}
	}

	// Try roman numerals
	m = romanRe.FindStringSubmatch(title)
	if m != nil {
		return romanToInt(strings.ToLower(m[1]))
	}

	return 0
}

func romanToInt(s string) int {
	roman := map[byte]int{'i': 1, 'v': 5, 'x': 10, 'l': 50, 'c': 100}
	result := 0
	for i := 0; i < len(s); i++ {
		if i+1 < len(s) && roman[s[i]] < roman[s[i+1]] {
			result -= roman[s[i]]
		} else {
			result += roman[s[i]]
		}
	}
	return result
}
