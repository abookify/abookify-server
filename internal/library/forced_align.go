// Forced alignment between a Whisper transcript and an ebook source text.
//
// Given two word sequences that represent "roughly the same content" (one
// from Whisper, one from the EPUB), produce a word-level mapping via
// Needleman-Wunsch global alignment. The mapping is then grouped into
// paragraph-level AlignmentPairs for the alignments table.
//
// The algorithm runs per chapter pair (via chapter_links), so each invocation
// handles ~3-6K words — well within O(n*m) DP feasibility.
package library

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Scoring constants for word alignment DP.
const (
	matchScore    = 2
	mismatchScore = -1
	gapScore      = -1
)

// wordMatch is one element in the DP traceback: which transcript word (if any)
// aligns with which ebook word (if any). -1 means a gap (insertion/deletion).
type wordMatch struct {
	ebookIdx      int // -1 = gap in ebook (transcript has extra word)
	transcriptIdx int // -1 = gap in transcript (ebook has extra word)
}

// alignWordsDP runs Needleman-Wunsch on two normalized word sequences and
// returns the optimal global alignment as a sequence of matched pairs.
//
// For sequences of length N and M, this uses O(N*M) time and space.
// Guard: if N*M > 50 million, returns nil (caller should chunk or skip).
func alignWordsDP(ebookNorm, transcriptNorm []string) []wordMatch {
	n := len(ebookNorm)
	m := len(transcriptNorm)
	if n == 0 || m == 0 {
		return nil
	}
	if int64(n)*int64(m) > 50_000_000 {
		log.Printf("forced-align: skipping DP for %d×%d words (too large)", n, m)
		return nil
	}

	// DP matrix: score[i][j] = best score aligning ebook[0:i] with transcript[0:j].
	// Use flat slice for cache efficiency.
	score := make([]int, (n+1)*(m+1))
	idx := func(i, j int) int { return i*(m+1) + j }

	// Initialize borders.
	for i := 1; i <= n; i++ {
		score[idx(i, 0)] = i * gapScore
	}
	for j := 1; j <= m; j++ {
		score[idx(0, j)] = j * gapScore
	}

	// Fill.
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			s := mismatchScore
			if ebookNorm[i-1] == transcriptNorm[j-1] {
				s = matchScore
			}
			diag := score[idx(i-1, j-1)] + s
			up := score[idx(i-1, j)] + gapScore   // gap in transcript
			left := score[idx(i, j-1)] + gapScore  // gap in ebook
			best := diag
			if up > best {
				best = up
			}
			if left > best {
				best = left
			}
			score[idx(i, j)] = best
		}
	}

	// Traceback.
	var matches []wordMatch
	i, j := n, m
	for i > 0 || j > 0 {
		if i > 0 && j > 0 {
			s := mismatchScore
			if ebookNorm[i-1] == transcriptNorm[j-1] {
				s = matchScore
			}
			if score[idx(i, j)] == score[idx(i-1, j-1)]+s {
				matches = append(matches, wordMatch{ebookIdx: i - 1, transcriptIdx: j - 1})
				i--
				j--
				continue
			}
		}
		if i > 0 && score[idx(i, j)] == score[idx(i-1, j)]+gapScore {
			matches = append(matches, wordMatch{ebookIdx: i - 1, transcriptIdx: -1})
			i--
		} else {
			matches = append(matches, wordMatch{ebookIdx: -1, transcriptIdx: j - 1})
			j--
		}
	}

	// Reverse (traceback produces them backwards).
	for a, b := 0, len(matches)-1; a < b; a, b = a+1, b-1 {
		matches[a], matches[b] = matches[b], matches[a]
	}
	return matches
}

// alignmentConfidence returns the fraction of ebook words that matched
// a transcript word (exact match after normalization).
func alignmentConfidence(matches []wordMatch, ebookNorm, transcriptNorm []string) float64 {
	if len(matches) == 0 {
		return 0
	}
	matched := 0
	total := 0
	for _, m := range matches {
		if m.ebookIdx >= 0 {
			total++
			if m.transcriptIdx >= 0 && ebookNorm[m.ebookIdx] == transcriptNorm[m.transcriptIdx] {
				matched++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(matched) / float64(total)
}

// ComputeTranscriptEbookAlignment runs forced alignment between a transcript
// book and an ebook book for a work. Aligns by chapter index: transcript
// chapter 0 ↔ ebook chapter 0, etc. Results are stored in the alignments
// table as a single alignment with paragraph-level pairs.
//
// Returns the number of chapter pairs aligned and the overall confidence.
func ComputeTranscriptEbookAlignment(store *db.Store, workID int64) (int, float64, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, 0, err
	}

	// Find the transcript and ebook books by authority.
	var transcriptBook, ebookBook *db.Book
	for i := range work.TextFiles {
		o := work.TextFiles[i].Origin
		if o == "whisper_transcript" && transcriptBook == nil {
			transcriptBook = &work.TextFiles[i]
		}
		if (o == "publisher_epub" || o == "publisher_mobi" || o == "publisher_pdf") &&
			(ebookBook == nil || db.OriginAuthority(o) > db.OriginAuthority(ebookBook.Origin)) {
			ebookBook = &work.TextFiles[i]
		}
	}
	if transcriptBook == nil || ebookBook == nil {
		return 0, 0, nil // nothing to align
	}

	// Load chapter lists for both books.
	tChapters, err := store.ListChapters(transcriptBook.ID)
	if err != nil {
		return 0, 0, err
	}
	eChapters, err := store.ListChapters(ebookBook.ID)
	if err != nil {
		return 0, 0, err
	}
	if len(tChapters) == 0 || len(eChapters) == 0 {
		return 0, 0, nil
	}

	// Build index maps for fast lookup.
	tByIdx := map[int]db.Chapter{}
	for _, ch := range tChapters {
		tByIdx[ch.Index] = ch
	}
	eByIdx := map[int]db.Chapter{}
	for _, ch := range eChapters {
		eByIdx[ch.Index] = ch
	}

	var allPairs []db.AlignmentPair
	totalConf := 0.0
	aligned := 0

	// Align each chapter pair where both sides exist.
	for idx := range eByIdx {
		tCh, tOK := tByIdx[idx]
		eCh := eByIdx[idx]
		if !tOK {
			continue
		}

		// Load full content.
		tFull, err := store.GetChapterContent(transcriptBook.ID, tCh.Index)
		if err != nil || tFull == nil || tFull.Content == "" {
			continue
		}
		eFull, err := store.GetChapterContent(ebookBook.ID, eCh.Index)
		if err != nil || eFull == nil || eFull.Content == "" {
			continue
		}

		// Tokenize and normalize.
		tWords := strings.Fields(tFull.Content)
		eWords := strings.Fields(eFull.Content)
		if len(tWords) == 0 || len(eWords) == 0 {
			continue
		}

		tNorm := make([]string, len(tWords))
		eNorm := make([]string, len(eWords))
		for i, w := range tWords {
			tNorm[i] = normalizeWord(w)
		}
		for i, w := range eWords {
			eNorm[i] = normalizeWord(w)
		}

		// Run DP alignment.
		matches := alignWordsDP(eNorm, tNorm)
		if matches == nil {
			log.Printf("forced-align: chapter %d skipped (too large: %d×%d)", idx, len(eWords), len(tWords))
			continue
		}
		conf := alignmentConfidence(matches, eNorm, tNorm)

		// Group matched words into paragraph-level pairs using ebook paragraphs.
		paragraphs, _ := store.ListParagraphs(ebookBook.ID, idx)
		if len(paragraphs) == 0 {
			// No paragraph data — emit one pair for the whole chapter.
			tStart, tEnd := transcriptRangeForEbookRange(matches, 0, len(eWords))
			if tStart >= 0 {
				allPairs = append(allPairs, db.AlignmentPair{
					FromChapter: idx, FromStart: 0, FromEnd: len(eWords),
					ToChapter: idx, ToStart: tStart, ToEnd: tEnd, Confidence: conf,
				})
			}
		} else {
			for _, p := range paragraphs {
				tStart, tEnd := transcriptRangeForEbookRange(matches, p.WordStart, p.WordEnd)
				if tStart < 0 {
					continue
				}
				allPairs = append(allPairs, db.AlignmentPair{
					FromChapter: idx, FromStart: p.WordStart, FromEnd: p.WordEnd,
					ToChapter: idx, ToStart: tStart, ToEnd: tEnd, Confidence: conf,
				})
			}
		}

		totalConf += conf
		aligned++
		log.Printf("forced-align: chapter %d: %d ebook words ↔ %d transcript words, conf=%.2f",
			idx, len(eWords), len(tWords), conf)
	}

	if aligned == 0 {
		return 0, 0, nil
	}

	avgConf := totalConf / float64(aligned)

	// Serialize pairs and store alignment.
	pairsJSON, err := json.Marshal(allPairs)
	if err != nil {
		return aligned, avgConf, err
	}

	if err := store.SaveAlignment(db.Alignment{
		WorkID:     workID,
		FromBookID: ebookBook.ID,
		ToBookID:   transcriptBook.ID,
		Unit:       "word",
		Confidence: avgConf,
		Method:     "edit-distance",
		Pairs:      string(pairsJSON),
	}); err != nil {
		return aligned, avgConf, err
	}

	log.Printf("forced-align: aligned %d chapters, avg conf=%.2f, %d paragraph pairs",
		aligned, avgConf, len(allPairs))
	return aligned, avgConf, nil
}

// transcriptRangeForEbookRange finds the min/max transcript indices that are
// matched to ebook words in the range [ebookStart, ebookEnd).
func transcriptRangeForEbookRange(matches []wordMatch, ebookStart, ebookEnd int) (int, int) {
	minT, maxT := -1, -1
	for _, m := range matches {
		if m.ebookIdx >= ebookStart && m.ebookIdx < ebookEnd && m.transcriptIdx >= 0 {
			if minT < 0 || m.transcriptIdx < minT {
				minT = m.transcriptIdx
			}
			if m.transcriptIdx+1 > maxT {
				maxT = m.transcriptIdx + 1
			}
		}
	}
	return minT, maxT
}
