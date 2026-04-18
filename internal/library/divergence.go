// Alignment divergence detection. Walks the alignment pairs stored by #63
// and identifies ebook paragraphs with little or no audio coverage —
// useful for detecting abridged audiobooks, skipped footnotes, or audio
// content not present in the ebook source.
package library

import (
	"encoding/json"

	"github.com/pj/abookify/internal/db"
)

// ParagraphCoverage summarizes how well a single ebook paragraph is covered
// by the aligned transcript.
type ParagraphCoverage struct {
	ChapterIdx   int     `json:"chapter_idx"`
	ParagraphIdx int     `json:"paragraph_idx"`
	WordCount    int     `json:"word_count"`    // ebook words in this paragraph
	Aligned      int     `json:"aligned_words"` // ebook words with a transcript match
	Confidence   float64 `json:"confidence"`    // confidence reported by the alignment pair
	Status       string  `json:"status"`        // "covered" | "partial" | "missing" | "unknown"
}

// DivergenceReport is the overall divergence summary for a work.
type DivergenceReport struct {
	WorkID              int64               `json:"work_id"`
	EbookBookID         int64               `json:"ebook_book_id"`
	TranscriptBookID    int64               `json:"transcript_book_id"`
	TotalParagraphs     int                 `json:"total_paragraphs"`
	CoveredParagraphs   int                 `json:"covered_paragraphs"`
	PartialParagraphs   int                 `json:"partial_paragraphs"`
	MissingParagraphs   int                 `json:"missing_paragraphs"`
	CoverageRatio       float64             `json:"coverage_ratio"` // covered / total
	OverallConfidence   float64             `json:"overall_confidence"`
	Paragraphs          []ParagraphCoverage `json:"paragraphs,omitempty"`
	Summary             string              `json:"summary"` // human-readable one-liner
}

// thresholds for classifying paragraph coverage
const (
	coveredConfidenceMin = 0.80 // ≥ 80% word match = covered
	partialConfidenceMin = 0.40 // 40-80% = partial
	// below 40% or no pair at all = missing
)

// ComputeDivergence walks the alignment for a work and produces a paragraph-by-
// paragraph coverage report. Requires that #63 has already been run.
// Returns nil (no error) if no alignment exists yet.
func ComputeDivergence(store *db.Store, workID int64) (*DivergenceReport, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, err
	}
	alignments, err := store.ListAlignmentsForWork(workID)
	if err != nil {
		return nil, err
	}
	if len(alignments) == 0 {
		return nil, nil
	}

	// Find the best alignment to use: prefer edit-distance between publisher
	// source and transcript. Skip whisper-native (that's audio↔transcript,
	// trivial).
	var chosen *db.Alignment
	for i := range alignments {
		a := &alignments[i]
		if a.Method == "edit-distance" {
			chosen = a
			break
		}
	}
	if chosen == nil {
		return nil, nil
	}

	var pairs []db.AlignmentPair
	if err := json.Unmarshal([]byte(chosen.Pairs), &pairs); err != nil {
		return nil, err
	}

	// Load all paragraphs for the ebook (from_book) side.
	ebookParagraphs, err := listAllParagraphsForBook(store, chosen.FromBookID)
	if err != nil {
		return nil, err
	}

	// Index pairs by (chapter, paragraph_word_start) for lookup.
	type key struct{ ch, ws, we int }
	pairByKey := map[key]db.AlignmentPair{}
	for _, p := range pairs {
		pairByKey[key{p.FromChapter, p.FromStart, p.FromEnd}] = p
	}

	report := &DivergenceReport{
		WorkID:           workID,
		EbookBookID:      chosen.FromBookID,
		TranscriptBookID: chosen.ToBookID,
		TotalParagraphs:  len(ebookParagraphs),
	}

	var confSum float64
	var confCount int

	for _, para := range ebookParagraphs {
		pair, ok := pairByKey[key{para.ChapterIdx, para.WordStart, para.WordEnd}]
		cov := ParagraphCoverage{
			ChapterIdx:   para.ChapterIdx,
			ParagraphIdx: para.ParagraphIdx,
			WordCount:    para.WordEnd - para.WordStart,
		}
		switch {
		case !ok:
			cov.Status = "missing"
			report.MissingParagraphs++
		case pair.Confidence >= coveredConfidenceMin:
			cov.Status = "covered"
			cov.Confidence = pair.Confidence
			cov.Aligned = pair.ToEnd - pair.ToStart
			report.CoveredParagraphs++
			confSum += pair.Confidence
			confCount++
		case pair.Confidence >= partialConfidenceMin:
			cov.Status = "partial"
			cov.Confidence = pair.Confidence
			cov.Aligned = pair.ToEnd - pair.ToStart
			report.PartialParagraphs++
			confSum += pair.Confidence
			confCount++
		default:
			cov.Status = "missing"
			cov.Confidence = pair.Confidence
			report.MissingParagraphs++
		}
		report.Paragraphs = append(report.Paragraphs, cov)
	}

	if report.TotalParagraphs > 0 {
		report.CoverageRatio = float64(report.CoveredParagraphs) / float64(report.TotalParagraphs)
	}
	if confCount > 0 {
		report.OverallConfidence = confSum / float64(confCount)
	}

	report.Summary = summarizeDivergence(report)
	return report, nil
}

func summarizeDivergence(r *DivergenceReport) string {
	if r.TotalParagraphs == 0 {
		return "no paragraphs to evaluate"
	}
	if r.CoverageRatio >= 0.95 {
		return "full coverage: audio reads the complete ebook"
	}
	if r.CoverageRatio >= 0.85 {
		return "near-complete coverage: minor sections skipped (footnotes, headers)"
	}
	if r.CoverageRatio >= 0.60 {
		return "partial coverage: likely abridged audio or significant divergence"
	}
	return "low coverage: audio and ebook differ substantially"
}

// listAllParagraphsForBook returns every paragraph across all chapters of a book,
// ordered by (chapter_idx, paragraph_idx).
func listAllParagraphsForBook(store *db.Store, bookID int64) ([]db.Paragraph, error) {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return nil, err
	}
	var all []db.Paragraph
	for _, ch := range chapters {
		paras, err := store.ListParagraphs(bookID, ch.Index)
		if err != nil {
			continue
		}
		all = append(all, paras...)
	}
	return all, nil
}
