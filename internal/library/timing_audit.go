package library

import (
	"fmt"
	"sort"

	"github.com/pj/abookify/internal/db"
)

// Chapter-timing audit — the check that would have caught the ebook word-time
// drift (fe2833d) on day one.
//
// Coverage numbers were IDENTICAL before and after a bug that put Lord of the
// Rings' last chapters 17 minutes late: coverage measures which words matched,
// not WHEN. The narration's own chapter structure is an independent clock —
// the narrator says "Chapter 11" at a known second (the detected audio chapter
// on canon.active.chapters_anchor_book_id, verified by listening), and the
// publisher chapter with the same number, timed through the alignment, must
// start within a few seconds of it. A drifting map, a timeline from the wrong
// edition, a mis-picked sync row: each moves that delta by minutes while every
// coverage figure stays put.
//
// Each alignment-timed ebook chapter is measured against the NEAREST spoken
// announcement. Matching by chapter number was tried first and cried wolf:
// ebook files numbered from the front matter ("Chapter 6" = the narrator's
// "Chapter 3"), audio detected as "Part N" against ebook "Chapter N" — all with
// perfect timing. The signal is the distance itself: a healthy pair sits 0–7 s
// from an announcement; a drifting map puts most chapters tens of seconds to
// minutes from any. A few far chapters with the rest on the mark are ebook
// units the narration has no announcement for (front matter, an appendix, a
// missed detection) and are listed as outliers, not a failure.

// chapterTimingToleranceSec: an EPUB chapter start is the first aligned word
// (the title line or the opening sentence); the audio chapter start is the
// announcement's first word. Real pairs sit 0–7 s apart (Selfish Gene median
// 2.2 s, Hitchhiker's 0 s). The old drift was 43 s on its smallest case.
const chapterTimingToleranceSec = 30.0

// minTimingMatches: chapters needed on both sides for a verdict.
const minTimingMatches = 3

// timingOKShare: the share of timed ebook chapters that must sit within
// tolerance of an announcement. A gradual drift (late chapters off, early ones
// fine) fails this before it moves the median.
const timingOKShare = 0.7

type ChapterTimingDelta struct {
	TextIndex     int     `json:"text_index"`
	TextTitle     string  `json:"text_title"`
	AudioIndex    int     `json:"audio_index"` // nearest announcement
	AudioStartSec float64 `json:"audio_start_sec"`
	TextStartSec  float64 `json:"text_start_sec"`
	DeltaSec      float64 `json:"delta_sec"` // text − nearest audio
}

type TimingReport struct {
	WorkID       int64                `json:"work_id"`
	Title        string               `json:"title"`
	AnchorBookID int64                `json:"anchor_book_id,omitempty"`
	TextBookID   int64                `json:"text_book_id,omitempty"`
	Skipped      string               `json:"skipped,omitempty"` // why no verdict
	Compared     int                  `json:"compared"`
	Within       int                  `json:"within_tolerance"`
	MedianAbsSec float64              `json:"median_abs_sec"`
	WorstAbsSec  float64              `json:"worst_abs_sec"`
	WorstTitle   string               `json:"worst_title,omitempty"`
	OK           bool                 `json:"ok"`
	Direction    string               `json:"direction,omitempty"` // which side was measured for the verdict
	Deltas       []ChapterTimingDelta `json:"deltas,omitempty"`
}

// AuditChapterTiming measures the authority publisher ebook's alignment-timed
// chapters against the active edition's detected audio chapters.
func AuditChapterTiming(store *db.Store, workID int64) (TimingReport, error) {
	rep := TimingReport{WorkID: workID}
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return rep, err
	}
	rep.Title = work.Title
	canon, err := BuildWorkCanon(store, workID)
	if err != nil || canon == nil || canon.Active.ChaptersAnchorBookID == 0 {
		rep.Skipped = "no active narration with detected chapters"
		return rep, nil
	}
	var ebook *db.Book
	for i := range work.TextFiles {
		b := &work.TextFiles[i]
		switch b.Origin {
		case "publisher_epub", "publisher_mobi", "publisher_pdf":
			if ebook == nil || db.OriginAuthority(b.Origin) > db.OriginAuthority(ebook.Origin) {
				ebook = b
			}
		}
	}
	if ebook == nil {
		rep.Skipped = "no publisher ebook"
		return rep, nil
	}
	rep.AnchorBookID, rep.TextBookID = canon.Active.ChaptersAnchorBookID, ebook.ID
	// A chain the reader does not trust (weakChainTranscript degrades it to the
	// transcript) has no timing to audit — its word map is not shown.
	if wc, err := BuildCoverage(store, workID); err == nil {
		for _, pc := range wc.Pairs {
			// A TTS edition's pair is provenance, not an alignment: it says
			// nothing about the narration chain under audit. Reading its
			// unmeasured in-text figure as 0 skipped every dual-edition work.
			if pc.ByConstruction {
				continue
			}
			if pc.Ebook.BookID == ebook.ID && pc.Unit == "word" && pc.AudioToEbookInText < minChainConfidence {
				rep.Skipped = fmt.Sprintf("weak chain (in-text quality %.2f) — the ebook highlight is not shown", pc.AudioToEbookInText)
				return rep, nil
			}
		}
	}

	audioChs, err := store.ListChapters(rep.AnchorBookID)
	if err != nil {
		return rep, err
	}
	if len(audioChs) < minTimingMatches {
		rep.Skipped = fmt.Sprintf("only %d detected audio chapters (need %d)", len(audioChs), minTimingMatches)
		return rep, nil
	}
	sort.Slice(audioChs, func(i, j int) bool { return audioChs[i].StartSec < audioChs[j].StartSec })
	ranges, err := EbookChapterAudioRanges(store, ebook.ID)
	if err != nil {
		return rep, err
	}
	if len(ranges) == 0 {
		rep.Skipped = "ebook has no alignment-timed chapters"
		return rep, nil
	}
	textChs, err := store.ListChapters(ebook.ID)
	if err != nil {
		return rep, err
	}
	type start struct {
		idx   int
		title string
		sec   float64
	}
	var textStarts []start
	for _, ch := range textChs {
		rng, timed := ranges[ch.Index]
		if !timed || rng[0] <= 0 {
			continue
		}
		textStarts = append(textStarts, start{ch.Index, ch.Title, rng[0]})
	}
	sort.Slice(textStarts, func(i, j int) bool { return textStarts[i].sec < textStarts[j].sec })
	// Every ebook chapter under one title is a running head stamped on spine
	// lumps (Kitchen Confidential: 27 × "Kitchen Confidential") — file splits,
	// not chapters; there is no structure to hold against the announcements.
	if len(textStarts) >= minTimingMatches {
		same := true
		for _, ts := range textStarts[1:] {
			if normalize(ts.title) != normalize(textStarts[0].title) {
				same = false
				break
			}
		}
		if same {
			rep.Skipped = fmt.Sprintf("ebook chapters carry no titles (%d × %q — spine lumps)", len(textStarts), textStarts[0].title)
			return rep, nil
		}
	}
	var audioStarts []start
	for _, ch := range audioChs {
		audioStarts = append(audioStarts, start{ch.Index, ch.Title, ch.StartSec})
	}
	nearest := func(from []start, to []start) []ChapterTimingDelta {
		var out []ChapterTimingDelta
		for _, f := range from {
			k := sort.Search(len(to), func(i int) bool { return to[i].sec >= f.sec })
			best := -1
			for _, cand := range []int{k - 1, k} {
				if cand < 0 || cand >= len(to) {
					continue
				}
				if best < 0 || absF(to[cand].sec-f.sec) < absF(to[best].sec-f.sec) {
					best = cand
				}
			}
			if best < 0 {
				continue
			}
			out = append(out, ChapterTimingDelta{TextIndex: f.idx, TextTitle: f.title, AudioIndex: to[best].idx,
				AudioStartSec: to[best].sec, TextStartSec: f.sec, DeltaSec: f.sec - to[best].sec})
		}
		return out
	}
	// Direction 1: every timed ebook chapter → nearest announcement (fails when
	// the narration announces fewer units than the ebook has). Direction 2:
	// every announcement → nearest timed ebook start (fails when the ebook is
	// coarser than the narration). A drifted map fails BOTH — every time is
	// wrong — so passing either is the honest verdict.
	rep.Deltas = nearest(textStarts, audioStarts)
	rep.summarize()
	if !rep.OK && rep.Skipped == "" {
		alt := TimingReport{}
		for _, d := range nearest(audioStarts, textStarts) {
			// from the audio side: TextTitle names the announcement
			alt.Deltas = append(alt.Deltas, d)
		}
		alt.summarize()
		if alt.OK {
			rep.Deltas, rep.Compared, rep.Within, rep.MedianAbsSec, rep.WorstAbsSec, rep.WorstTitle, rep.OK =
				alt.Deltas, alt.Compared, alt.Within, alt.MedianAbsSec, alt.WorstAbsSec, alt.WorstTitle, true
			rep.Direction = "announcement→ebook"
		}
	}
	if rep.Direction == "" && rep.Skipped == "" {
		rep.Direction = "ebook→announcement"
	}
	return rep, nil
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// summarize fills Compared/Within/Median/Worst/OK (or Skipped) from Deltas.
func (rep *TimingReport) summarize() {
	rep.Compared = len(rep.Deltas)
	if rep.Compared < minTimingMatches {
		rep.Skipped = fmt.Sprintf("only %d timed ebook chapters (need %d)", rep.Compared, minTimingMatches)
		return
	}
	abs := make([]float64, 0, rep.Compared)
	rep.Within, rep.WorstAbsSec, rep.WorstTitle = 0, 0, ""
	for _, d := range rep.Deltas {
		a := absF(d.DeltaSec)
		abs = append(abs, a)
		if a <= chapterTimingToleranceSec {
			rep.Within++
		}
		if a > rep.WorstAbsSec {
			rep.WorstAbsSec, rep.WorstTitle = a, d.TextTitle
		}
	}
	sort.Float64s(abs)
	rep.MedianAbsSec = abs[len(abs)/2]
	rep.OK = rep.MedianAbsSec <= chapterTimingToleranceSec &&
		float64(rep.Within) >= timingOKShare*float64(rep.Compared)
}
