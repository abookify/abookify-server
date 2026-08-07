package library

import (
	"encoding/json"
	"log"
	"regexp"
	"sort"
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

	// Preferred path for multi-file audiobooks: derive each file's text chapter
	// from the word alignment's audio timeline, not the file title. Title
	// matching COLLAPSES when every file is named by the book title (no
	// "Chapter N") — the word-overlap fallback then maps all files onto the one
	// text chapter that shares the most title words (Hitchhiker's: 5 files all →
	// text 8). The alignment already knows which text region plays at each
	// second, so map file.StartSec → text chapter directly.
	if derived, ok := linkChaptersByAlignment(store, work); ok {
		if err := store.DeleteChapterLinksByWork(work.ID); err != nil {
			return err
		}
		for _, l := range derived {
			if err := store.InsertChapterLink(work.ID, l); err != nil {
				return err
			}
		}
		log.Printf("linked %d audio files to text via alignment for %q",
			len(derived), work.Title)
		return nil
	}

	// Clean slate — old links may reference a different chapter count/layout.
	if err := store.DeleteChapterLinksByWork(work.ID); err != nil {
		return err
	}

	links := make([]db.ChapterLink, 0, len(audioChapters))
	for _, ac := range audioChapters {
		bestTextIdx, confidence := matchTextChapter(ac.title, textByNum, textByNorm, textChapters)
		if bestTextIdx < 0 {
			continue
		}
		links = append(links, db.ChapterLink{
			AudioBookID: ac.bookID,
			AudioIndex:  ac.idx,
			TextBookID:  textBookID,
			TextIndex:   bestTextIdx,
			Confidence:  confidence,
		})
	}

	// Collapse guard: title matching that maps 2+ distinct audio chapters onto a
	// SINGLE text index is noise, not a real mapping (the degenerate all→one
	// case). Writing it is worse than writing nothing — the reader follows every
	// audio chapter to the same wrong text chapter and renders grey. Drop the set
	// and leave the reader to its timing-derived chapter follow, which is honest.
	if collapsedToOneIndex(links) {
		log.Printf("skipped chapter links for %q: title match collapsed %d audio chapters onto one text index",
			work.Title, len(links))
		return nil
	}

	for _, l := range links {
		if err := store.InsertChapterLink(work.ID, l); err != nil {
			return err
		}
	}
	if len(links) > 0 {
		log.Printf("linked %d/%d audio chapters to text for %q",
			len(links), len(audioChapters), work.Title)
	}
	return nil
}

// collapsedToOneIndex reports whether every link points at the same text index
// while spanning 2+ audio chapters — the degenerate title-match outcome.
func collapsedToOneIndex(links []db.ChapterLink) bool {
	if len(links) < 2 {
		return false
	}
	first := links[0].TextIndex
	for _, l := range links[1:] {
		if l.TextIndex != first {
			return false
		}
	}
	return true
}

// linkChaptersByAlignment derives chapter links for a multi-file audiobook from
// the word alignment between the display ebook and the transcript. For each
// audio file it finds the ebook chapter playing at the file's StartSec (its
// offset on the concatenated book-audio timeline), giving one distinct,
// monotonic link per file — the coarse nav anchor the reader follows. Returns
// (nil, false) when not applicable (single-file work, no word alignment, no
// per-file StartSec, or no per-chapter audio timing), so the caller falls back
// to title matching. Read-only consumer of transcription's alignment payload,
// like text_sync.go / diff.go.
func linkChaptersByAlignment(store *db.Store, work *db.Work) ([]db.ChapterLink, bool) {
	if len(work.AudioFiles) < 2 {
		return nil, false
	}

	// The ebook side of the alignment = the non-transcript text book with
	// chapters (what the reader displays and what EbookChapters index against).
	transIDs := map[int64]bool{}
	var ebookID int64
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			transIDs[b.ID] = true
		} else if ebookID == 0 && b.ChapterCount > 0 {
			ebookID = b.ID
		}
	}
	if ebookID == 0 {
		return nil, false
	}

	aligns, err := store.ListAlignmentsForWork(work.ID)
	if err != nil {
		return nil, false
	}
	var best *db.Alignment
	for i := range aligns {
		a := &aligns[i]
		if a.Unit != "word" {
			continue
		}
		paired := (a.FromBookID == ebookID && transIDs[a.ToBookID]) ||
			(a.ToBookID == ebookID && transIDs[a.FromBookID])
		if !paired {
			continue
		}
		if best == nil || a.Confidence > best.Confidence {
			best = a
		}
	}
	if best == nil {
		return nil, false
	}
	var p AnchorAlignmentPayload
	if json.Unmarshal([]byte(best.Pairs), &p) != nil || len(p.EbookChapters) == 0 {
		return nil, false
	}

	// Earliest audio second for each ebook chapter, from aligned segments.
	chapStart := map[int]float64{}
	for _, s := range p.Segments {
		if s.Kind != SegAligned || s.StartSec <= 0 {
			continue
		}
		ci := chapterOfToken(p.EbookChapters, s.EbookStart)
		if ci < 0 {
			continue
		}
		if cur, ok := chapStart[ci]; !ok || s.StartSec < cur {
			chapStart[ci] = s.StartSec
		}
	}
	if len(chapStart) == 0 {
		return nil, false
	}
	// (audio second → chapter index), ascending, so a file's StartSec picks the
	// chapter playing at that moment.
	type tp struct {
		sec float64
		idx int
	}
	timeline := make([]tp, 0, len(chapStart))
	for ci, sec := range chapStart {
		timeline = append(timeline, tp{sec, ci})
	}
	sort.Slice(timeline, func(i, j int) bool { return timeline[i].sec < timeline[j].sec })

	links := make([]db.ChapterLink, 0, len(work.AudioFiles))
	for i, af := range work.AudioFiles {
		// Greatest timeline entry whose sec <= this file's start.
		pick := timeline[0].idx
		for _, t := range timeline {
			if t.sec <= af.StartSec {
				pick = t.idx
			} else {
				break
			}
		}
		links = append(links, db.ChapterLink{
			AudioBookID: af.ID,
			AudioIndex:  i,
			TextBookID:  ebookID,
			TextIndex:   pick,
			Confidence:  0.85, // alignment-derived: authoritative, above title-overlap
		})
	}
	return links, true
}

// chapterOfToken returns the ebook chapter index whose token range contains tok,
// or -1. Chapters partition the token stream contiguously.
func chapterOfToken(chapters []ChapterSpan, tok int) int {
	for _, ch := range chapters {
		if tok >= ch.Start && tok < ch.Start+ch.Len {
			return ch.Index
		}
	}
	return -1
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
