package library

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"github.com/pj/abookify/internal/db"
)

// Card 39 (2026-09-23): the narration's chapter rows began hours into the
// book — Dracula's list opened at CHAPTER VI (2:33:47), Pride and Prejudice's
// at chapter 36 (6.9 h in), Peter Pan's at VI — because the detector's first
// ACCEPTED announcement came late and nothing filled the rows before it,
// although the alignment knew to the second where chapters I–V begin. The
// transcript carried the same gap as one lump row.
//
// FillLeadingRows completes the lead: for a work whose anchor audio rows start
// more than leadRowSecs in, every ebook chapter the (fresh, trusted) alignment
// places before that first row becomes a row of its own — on the audio anchor
// (inserted) and on the transcript (the lump replaced, content sliced from the
// narration's word timeline). Existing rows keep their titles and times and
// move down; links and paragraphs are rebuilt; the work's content version
// bumps so the phone refreshes. Idempotent: afterwards the first row starts at
// 0 and the function is a no-op.

const (
	leadRowSecs     = 600.0 // a list that opens later than this is "late"
	leadMinRangeSec = 60.0  // a chapter narrated for less than this (a title page) is not a row
	leadMinConf     = 0.9
)

// LeadingRowsReport says what FillLeadingRows did or would do for one work.
type LeadingRowsReport struct {
	WorkID       int64    `json:"work_id"`
	AudioBookID  int64    `json:"audio_book_id"`
	TranscriptID int64    `json:"transcript_id"`
	FirstRowSec  float64  `json:"first_row_sec"`
	Added        []string `json:"added"` // "0h 00m 11s  CHAPTER I …"
	Applied      bool     `json:"applied"`
	Skipped      string   `json:"skipped,omitempty"`
}

// FillLeadingRows completes the lead rows of a work from its ebook alignment.
func FillLeadingRows(store *db.Store, work *db.Work, ebookID int64, dryRun bool) (LeadingRowsReport, error) {
	rep := LeadingRowsReport{WorkID: work.ID}
	// The narration book that carries the chapter rows the apps render: the
	// earliest audio file with rows. TTS editions carry none.
	var anchor *db.Book
	var anchorRows []db.Chapter
	for i := range work.AudioFiles {
		b := &work.AudioFiles[i]
		if b.Origin == "tts_kokoro" {
			continue
		}
		rows, err := store.ListChapters(b.ID)
		if err != nil || len(rows) == 0 {
			continue
		}
		if anchor == nil || b.StartSec < anchor.StartSec {
			anchor, anchorRows = b, rows
		}
	}
	if anchor == nil {
		rep.Skipped = "no narration book carries chapter rows"
		return rep, nil
	}
	rep.AudioBookID = anchor.ID
	sort.Slice(anchorRows, func(i, j int) bool { return anchorRows[i].StartSec < anchorRows[j].StartSec })
	first := anchorRows[0].StartSec
	rep.FirstRowSec = first
	if first <= leadRowSecs {
		rep.Skipped = "rows already begin at the start"
		return rep, nil
	}

	// The alignment: fresh (EbookChapterAudioRanges withholds a stale one) and trusted.
	conf, transcriptID := bestEbookTranscriptAlignment(store, work, ebookID)
	if transcriptID == 0 {
		rep.Skipped = "no ebook↔transcript word alignment"
		return rep, nil
	}
	rep.TranscriptID = transcriptID
	if conf < leadMinConf {
		rep.Skipped = fmt.Sprintf("alignment confidence %.2f below %.2f", conf, leadMinConf)
		return rep, nil
	}
	ranges, err := EbookChapterAudioRangesFor(store, ebookID, transcriptID)
	if err != nil {
		return rep, err
	}
	if len(ranges) == 0 {
		rep.Skipped = "no chapter ranges (alignment stale or unbaked)"
		return rep, nil
	}
	// The anchor must belong to the narration this transcript's clock describes.
	maxSec := 0.0
	for _, r := range ranges {
		if r[1] > maxSec {
			maxSec = r[1]
		}
	}
	if members := narrationFilesForTimeline(store, work, maxSec); members != nil && !members[anchor.ID] {
		rep.Skipped = fmt.Sprintf("anchor book %d is not in the narration the transcript %d describes (two narrations)", anchor.ID, transcriptID)
		return rep, nil
	}
	ebookChs, err := store.ListChapters(ebookID)
	if err != nil {
		return rep, err
	}
	type lead struct {
		start, end float64
		title      string
	}
	var leads []lead
	for _, ch := range ebookChs {
		r, ok := ranges[ch.Index]
		if !ok || r[1]-r[0] < leadMinRangeSec || r[0] >= first-30 {
			continue
		}
		leads = append(leads, lead{r[0], r[1], normalizeChapterTitle(ch.Title)})
	}
	if len(leads) == 0 {
		rep.Skipped = "no ebook chapter is aligned before the first row"
		return rep, nil
	}
	sort.Slice(leads, func(i, j int) bool { return leads[i].start < leads[j].start })
	// Rows tile the lead: the first opens at 0, each ends where the next begins,
	// the last ends at the first existing row.
	newRows := make([]db.Chapter, 0, len(leads))
	for i, l := range leads {
		start := l.start
		if i == 0 {
			start = 0
		}
		end := first
		if i+1 < len(leads) {
			end = leads[i+1].start
		}
		newRows = append(newRows, db.Chapter{Title: l.title, Src: "aligned", StartSec: start, EndSec: end, Confidence: conf})
		rep.Added = append(rep.Added, fmt.Sprintf("%s  %s", hms(start), l.title))
	}

	// Transcript: rows that lie wholly inside the lead are the lump to replace.
	transRows, err := store.ListChaptersWithContent(transcriptID)
	if err != nil {
		return rep, err
	}
	sort.Slice(transRows, func(i, j int) bool { return transRows[i].StartSec < transRows[j].StartSec })
	var keepTrans []db.Chapter
	for _, r := range transRows {
		if r.EndSec <= first+2 {
			continue // inside the lead: replaced
		}
		if r.StartSec < first-2 {
			rep.Skipped = fmt.Sprintf("transcript row %d straddles the first anchor row (%.0f–%.0f vs %.0f)", r.Index, r.StartSec, r.EndSec, first)
			return rep, nil
		}
		keepTrans = append(keepTrans, r)
	}
	words := anchorWordTimeline(store, work, anchor.ID)
	if len(words) == 0 || words[len(words)-1].End < first {
		rep.Skipped = "no book-continuous word timeline to slice transcript rows from"
		return rep, nil
	}
	if dryRun {
		return rep, nil
	}

	// Audio anchor: new rows, then the existing ones, renumbered.
	audioOut := append([]db.Chapter{}, newRows...)
	audioOut = append(audioOut, anchorRows...)
	if err := rewriteRows(store, anchor.ID, audioOut); err != nil {
		return rep, fmt.Errorf("audio rows: %w", err)
	}
	// Transcript: new rows with sliced content, then the kept ones.
	transOut := make([]db.Chapter, 0, len(newRows)+len(keepTrans))
	for _, r := range newRows {
		slice := wordsBetween(words, r.StartSec, r.EndSec)
		r.Content = joinWords(slice)
		r.WordCount = len(slice)
		transOut = append(transOut, r)
	}
	transOut = append(transOut, keepTrans...)
	if err := rewriteRows(store, transcriptID, transOut); err != nil {
		return rep, fmt.Errorf("transcript rows: %w", err)
	}
	if _, err := PopulateParagraphsForBook(store, transcriptID); err != nil {
		log.Printf("leading-rows: paragraphs for book %d: %v", transcriptID, err)
	}
	if fresh, err := store.GetWork(work.ID); err == nil && fresh != nil {
		if err := LinkChapters(store, fresh); err != nil {
			log.Printf("leading-rows: relink work %d: %v", work.ID, err)
		}
	}
	if err := store.BumpContentVersion(work.ID); err != nil {
		log.Printf("leading-rows: bump content version for work %d: %v", work.ID, err)
	}
	rep.Applied = true
	log.Printf("leading-rows: work %d: %d chapter row(s) added before %s on audio book %d and transcript %d", work.ID, len(newRows), hms(first), anchor.ID, transcriptID)
	return rep, nil
}

// bestEbookTranscriptAlignment: the highest-confidence word alignment between
// the ebook and any transcript of the work, and that transcript's id.
func bestEbookTranscriptAlignment(store *db.Store, work *db.Work, ebookID int64) (float64, int64) {
	trans := map[int64]bool{}
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			trans[b.ID] = true
		}
	}
	aligns, err := store.ListAlignmentsForWork(work.ID)
	if err != nil {
		return 0, 0
	}
	var conf float64
	var tid int64
	for _, a := range aligns {
		if a.Unit != "word" {
			continue
		}
		var other int64
		switch {
		case a.FromBookID == ebookID && trans[a.ToBookID]:
			other = a.ToBookID
		case a.ToBookID == ebookID && trans[a.FromBookID]:
			other = a.FromBookID
		default:
			continue
		}
		if tid == 0 || a.Confidence > conf {
			conf, tid = a.Confidence, other
		}
	}
	return conf, tid
}

// anchorWordTimeline: the anchor file's word timeline, which for a multi-file
// narration holds the whole chain in book-continuous seconds.
func anchorWordTimeline(store *db.Store, work *db.Work, anchorID int64) []db.SyncTimestamp {
	raw, err := store.GetSyncData(work.ID, anchorID, 0)
	if err != nil || raw == "" {
		return nil
	}
	var ts []db.SyncTimestamp
	if json.Unmarshal([]byte(raw), &ts) != nil {
		return nil
	}
	return ts
}

func wordsBetween(words []db.SyncTimestamp, from, to float64) []db.SyncTimestamp {
	lo := sort.Search(len(words), func(i int) bool { return words[i].Start >= from })
	hi := sort.Search(len(words), func(i int) bool { return words[i].Start >= to })
	if hi < lo {
		hi = lo
	}
	return words[lo:hi]
}

// rewriteRows replaces a book's chapter rows with rows, renumbered in order.
func rewriteRows(store *db.Store, bookID int64, rows []db.Chapter) error {
	if err := store.DeleteChaptersByBook(bookID); err != nil {
		return err
	}
	for i := range rows {
		rows[i].BookID = bookID
		rows[i].Index = i
		if err := store.InsertChapter(rows[i]); err != nil {
			return err
		}
	}
	return nil
}

func hms(sec float64) string {
	s := int(sec + 0.5)
	return fmt.Sprintf("%dh %02dm %02ds", s/3600, (s%3600)/60, s%60)
}
