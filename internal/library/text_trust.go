package library

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Making untrustworthy text VISIBLE.
//
// The detectors know which books contain passages that do not match the audio.
// Until this existed, no human using the product did — a book with 2,139 invented
// words looked exactly like a clean one, in the library, the reader and Q&A.
//
// Follows the honest-badge principle already used for alignment coverage: state
// the suspicion plainly, neither hiding it nor dressing it up as proof. Low
// transcriber confidence is EVIDENCE, not a verdict — genuinely hard audio
// (accents, crosstalk) also produces it on correct text. So the copy says "may not
// match", the raw counts are always included so a reader can judge scale, and
// "never checked" is a distinct state rather than being folded into "fine".

// Trust states. Deliberately includes Unchecked: a book nobody has examined must
// not render like a book that passed.
const (
	TrustVerified    = "verified"    // checked, nothing found
	TrustMinor       = "minor"       // checked, under 1% of words suspect
	TrustSignificant = "significant" // checked, 1% or more suspect
	TrustUnchecked   = "unchecked"   // no confidence data — question not askable
)

// trustSignificantPct is where "minor" becomes "significant" — and, with the
// presentation rule, where a client surfaces the warning banner. Set to 3%: the
// coverage number already tells the reader the text isn't perfect (a 94%-aligned
// work is visibly not 100%), so a second warning below 3% is redundant noise on
// top of a number that's already honest. Below 3% the information still exists in
// the review area; it just doesn't interrupt anyone. The raw counts are always
// exposed so the UI never relies on the tier alone.
const trustSignificantPct = 3.0

// TrustChapter is one affected chapter, so a reader can be told WHERE rather than
// only that something somewhere is wrong.
type TrustChapter struct {
	Index        int    `json:"index"`
	Title        string `json:"title"`
	SuspectWords int    `json:"suspect_words"`
}

// TrustPassage is one actual low-confidence run — the words themselves plus the
// time to hear them — so a reader can tap it, play it, and judge for themselves
// instead of trusting a count. This is what the chapter breakdown couldn't give.
type TrustPassage struct {
	ChapterIndex int     `json:"chapter_index"`
	ChapterTitle string  `json:"chapter_title,omitempty"`
	StartSec     float64 `json:"start_sec"`
	EndSec       float64 `json:"end_sec"`
	WordCount    int     `json:"word_count"`
	Text         string  `json:"text"`
}

// TextTrust is the per-work payload.
type TextTrust struct {
	WorkID         int64          `json:"work_id"`
	State          string         `json:"state"`
	CheckedAt      string         `json:"checked_at,omitempty"`
	SuspectWords   int            `json:"suspect_words"`
	TotalWords     int            `json:"total_words"`
	SuspectPercent float64        `json:"suspect_percent"`
	WorstAtSec     float64        `json:"worst_at_sec,omitempty"`
	Chapters       []TrustChapter `json:"chapters,omitempty"`
	Passages       []TrustPassage `json:"passages,omitempty"`
	Headline       string         `json:"headline"`
	Detail         string         `json:"detail"`
}

// trustState derives the tier.
func trustState(hasConf bool, suspect, total int) string {
	if !hasConf {
		return TrustUnchecked
	}
	if suspect == 0 {
		return TrustVerified
	}
	if total > 0 && 100*float64(suspect)/float64(total) >= trustSignificantPct {
		return TrustSignificant
	}
	return TrustMinor
}

// trustCopy returns the headline and detail. Server-owned so web and mobile say
// the same thing, exactly as the gap-status legend does — and so the wording
// cannot drift into overstating a suspicion as a finding.
func trustCopy(state string, suspect, total int, pct float64) (string, string) {
	switch state {
	case TrustUnchecked:
		return "Not checked against the audio",
			"This transcript predates per-word confidence data, so we cannot tell whether its " +
				"text matches the recording. That is not the same as it being fine."
	case TrustVerified:
		return "Text matches the audio",
			fmt.Sprintf("All %s words were transcribed with high confidence.", commaInt(total))
	case TrustSignificant:
		return "Some of this text may not match the audio",
			fmt.Sprintf("%s of %s words (%.1f%%) sit in passages where the transcriber's confidence "+
				"collapsed. In this library that has meant the text was invented rather than heard. "+
				"Playback is unaffected; the reader, search and Q&A may show wording the narrator "+
				"never said.", commaInt(suspect), commaInt(total), pct)
	default: // minor — lead with the truth (nearly all matches); a low % overstated
		// as a warning is its own dishonesty. Not shown as a banner (under 1%),
		// available for the curious.
		return "Nearly all of this matches the audio",
			fmt.Sprintf("%s of %s words (%.2f%%) were transcribed with low confidence — a few "+
				"scattered phrases that may not be exactly what the narrator said. The rest matches.",
				commaInt(suspect), commaInt(total), pct)
	}
}

func commaInt(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// BuildTextTrust renders a stored row (or its absence) into the API payload.
func BuildTextTrust(workID int64, row *db.TextTrustRow) TextTrust {
	out := TextTrust{WorkID: workID, State: TrustUnchecked}
	if row == nil {
		out.Headline, out.Detail = "Not checked against the audio",
			"This book has not been examined yet, so we cannot say whether its text matches the "+
				"recording. That is not the same as it being fine."
		return out
	}
	out.CheckedAt = row.CheckedAt
	out.SuspectWords = row.SuspectWords
	out.TotalWords = row.TotalWords
	out.WorstAtSec = row.WorstAtSec
	if row.TotalWords > 0 {
		out.SuspectPercent = 100 * float64(row.SuspectWords) / float64(row.TotalWords)
	}
	out.State = trustState(row.HasConfidence, row.SuspectWords, row.TotalWords)
	out.Headline, out.Detail = trustCopy(out.State, row.SuspectWords, row.TotalWords, out.SuspectPercent)
	if row.ChaptersJSON != "" {
		json.Unmarshal([]byte(row.ChaptersJSON), &out.Chapters)
	}
	if row.PassagesJSON != "" {
		json.Unmarshal([]byte(row.PassagesJSON), &out.Passages)
	}
	return out
}

// maxTrustPassages caps the stored passage list — worst (longest) runs first, so
// a reader sees the passages most likely to be wrong without an unbounded blob.
const maxTrustPassages = 50

// lowConfidencePassages returns the actual low-confidence runs as playable
// passages: the run's own words, its [start,end] audio time, and the chapter it
// falls in. Same single walk as the counts (cheap — no extra sidecar read), just
// capturing the text + times instead of only tallying. Worst-first, capped, with
// each passage's text bounded so the payload stays small.
func lowConfidencePassages(store *db.Store, w *db.Work, words []sttWord) []TrustPassage {
	// Chapter lookup by start time (transcript chapters carry StartSec/EndSec).
	var chs []db.Chapter
	for _, b := range w.TextFiles {
		if b.Format == "transcript" || b.Origin == "whisper_transcript" {
			chs, _ = store.ListChapters(b.ID)
			break
		}
	}
	chapterAt := func(sec float64) (int, string) {
		for _, c := range chs {
			if sec >= c.StartSec && (c.EndSec == 0 || sec < c.EndSec) {
				return c.Index, c.Title
			}
		}
		return -1, ""
	}

	var out []TrustPassage
	var run []sttWord
	flush := func() {
		if len(run) >= minLowConfRun {
			idx, title := chapterAt(run[0].Start)
			out = append(out, TrustPassage{
				ChapterIndex: idx, ChapterTitle: title,
				StartSec: run[0].Start, EndSec: run[len(run)-1].End,
				WordCount: len(run), Text: passageText(run),
			})
		}
		run = nil
	}
	for _, x := range words {
		if x.Probability < lowConfFloor {
			run = append(run, x)
		} else {
			flush()
		}
	}
	flush()

	// Worst (longest) first, capped.
	sort.Slice(out, func(i, j int) bool { return out[i].WordCount > out[j].WordCount })
	if len(out) > maxTrustPassages {
		out = out[:maxTrustPassages]
	}
	return out
}

// passageText joins a run's words and bounds the length so one pathological run
// can't bloat the stored payload; the reader plays from the timestamp for the rest.
func passageText(run []sttWord) string {
	const maxRunes = 400
	parts := make([]string, len(run))
	for i, x := range run {
		parts[i] = x.Word
	}
	s := strings.TrimSpace(strings.Join(parts, " "))
	if len([]rune(s)) > maxRunes {
		s = string([]rune(s)[:maxRunes]) + "…"
	}
	return s
}

// ComputeTextTrust examines a work's sidecar and persists the verdict, attributing
// suspect words to chapters so the UI can say WHERE.
func ComputeTextTrust(store *db.Store, libraryRoot string, workID int64) (*TextTrust, error) {
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		return nil, err
	}
	if len(w.AudioFiles) == 0 {
		return nil, nil
	}
	path := findSidecar(w.AudioFiles[0].Path, libraryRoot)
	if path == "" {
		return nil, nil // never transcribed
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc sttSidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, err
	}
	if len(sc.Words) == 0 {
		return nil, nil
	}

	row := db.TextTrustRow{
		WorkID:        workID,
		HasConfidence: hasConf(sc.Words),
		TotalWords:    len(sc.Words),
	}
	if row.HasConfidence {
		n, at := lowConfidenceRun(sc.Words)
		row.SuspectWords, row.WorstAtSec = n, at
		if n > 0 {
			if enc, e := json.Marshal(attributeToChapters(store, w, sc.Words)); e == nil {
				row.ChaptersJSON = string(enc)
			}
			if enc, e := json.Marshal(lowConfidencePassages(store, w, sc.Words)); e == nil {
				row.PassagesJSON = string(enc)
			}
		}
	}
	if err := store.SaveTextTrust(row); err != nil {
		return nil, err
	}
	t := BuildTextTrust(workID, &row)
	return &t, nil
}

// attributeToChapters maps suspect words onto transcript chapters by time, so the
// reader can be pointed at the affected chapters rather than the whole book.
func attributeToChapters(store *db.Store, w *db.Work, words []sttWord) []TrustChapter {
	var tb int64
	for _, b := range w.TextFiles {
		if b.Format == "transcript" || b.Origin == "whisper_transcript" {
			tb = b.ID
			break
		}
	}
	if tb == 0 {
		return nil
	}
	chs, err := store.ListChapters(tb)
	if err != nil || len(chs) == 0 {
		return nil
	}
	counts := map[int]int{}
	titles := map[int]string{}
	for _, c := range chs {
		titles[c.Index] = c.Title
	}
	// Walk the suspect runs and bucket each word into its chapter by start time.
	var cur int
	var run []sttWord
	flush := func() {
		if cur >= minLowConfRun {
			for _, x := range run {
				for _, c := range chs {
					if x.Start >= c.StartSec && (c.EndSec == 0 || x.Start < c.EndSec) {
						counts[c.Index]++
						break
					}
				}
			}
		}
		cur, run = 0, nil
	}
	for _, x := range words {
		if x.Probability < lowConfFloor {
			cur++
			run = append(run, x)
		} else {
			flush()
		}
	}
	flush()

	out := make([]TrustChapter, 0, len(counts))
	for idx, n := range counts {
		out = append(out, TrustChapter{Index: idx, Title: titles[idx], SuspectWords: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SuspectWords > out[j].SuspectWords })
	if len(out) > 20 { // cap the payload; worst first
		out = out[:20]
	}
	return out
}
