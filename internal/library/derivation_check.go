package library

import (
	"encoding/json"
	"fmt"

	"github.com/pj/abookify/internal/db"
)

// Post-derivation completeness verification. The failure this exists to catch is
// a derivation chain (align → link → sync) that RAN, reported success, and left
// the data partial — collapsed chapter links, or a narration synced only part of
// the way. Re-deriving on update only helps if something afterwards asserts the
// result is whole; VerifyWorkDerivation is that assertion, and it runs
// library-wide (StartupDerivationSweep) so a work already in a bad state is
// found rather than discovered one screen at a time.

// DerivationIssue is one concrete way a work's derived karaoke data is partial.
type DerivationIssue struct {
	Kind   string `json:"kind"` // links_collapsed | sync_missing | sync_partial
	Detail string `json:"detail"`
}

// DerivationReport is the completeness verdict for one work. OK=false means the
// chain left something half-finished — loud, instead of grey text the user has
// to notice.
type DerivationReport struct {
	WorkID          int64             `json:"work_id"`
	Title           string            `json:"title"`
	KaraokeExpected bool              `json:"karaoke_expected"`
	OK              bool              `json:"ok"`
	Issues          []DerivationIssue `json:"issues,omitempty"`
}

// syncCoverageFloor: a narration must be synced to at least this fraction of its
// duration to count as complete. Real full syncs land at ~99.9% (the last word
// ends a beat before the audio does); a genuinely half-finished transcription
// stops far below this.
const syncCoverageFloor = 0.9

// VerifyWorkDerivation asserts a work's derived karaoke data is complete:
// chapter links are not collapsed onto one text index, and — when karaoke is
// expected (a word alignment exists) — every narration is synced to its end.
func VerifyWorkDerivation(store *db.Store, work *db.Work) (DerivationReport, error) {
	rep := DerivationReport{WorkID: work.ID, Title: work.Title, OK: true}
	if !work.HasAudio || !work.HasText {
		return rep, nil // nothing linkable/syncable to be incomplete
	}

	// 1. Chapter links must never collapse onto a single text index (every audio
	// chapter following to the same text chapter → wrong chapter → grey text).
	links, err := store.GetChapterLinks(work.ID)
	if err != nil {
		return rep, err
	}
	if collapsedToOneIndex(links) {
		rep.OK = false
		rep.Issues = append(rep.Issues, DerivationIssue{
			Kind:   "links_collapsed",
			Detail: fmt.Sprintf("%d audio chapters all link to text index %d", len(links), links[0].TextIndex),
		})
	}

	// Karaoke is EXPECTED only when a word alignment pairs the ebook with a
	// transcript. Without one, absent sync is correct (audio-only, a different
	// edition, an un-narrated ebook) — not a half-finished chain — so sync is not
	// asserted for those works.
	aligns, err := store.ListAlignmentsForWork(work.ID)
	if err != nil {
		return rep, err
	}
	for _, a := range aligns {
		if a.Unit == "word" {
			rep.KaraokeExpected = true
			break
		}
	}
	if !rep.KaraokeExpected {
		return rep, nil
	}

	// 2. Every narration must be synced to its end. Sync is stored one row per
	// narration, keyed by the narration's first audio file; the row's word
	// timeline (max end-sec) must reach the narration's end. Two narrations of
	// the same book (a human and an AI reading) each get their own row and their
	// own chain — summing them was the false positive this per-narration model
	// avoids.
	syncRows, err := store.ListSyncForWork(work.ID)
	if err != nil {
		return rep, err
	}
	if len(syncRows) == 0 {
		rep.OK = false
		rep.Issues = append(rep.Issues, DerivationIssue{Kind: "sync_missing", Detail: "word-aligned work has no sync_data"})
		return rep, nil
	}
	span := map[int64]float64{} // representative book → furthest synced second
	for _, row := range syncRows {
		var ws []SyncWord
		if json.Unmarshal([]byte(row.Timestamps), &ws) != nil {
			continue
		}
		for _, wd := range ws {
			if wd.E > span[row.AudioBookID] {
				span[row.AudioBookID] = wd.E
			}
		}
	}
	for repBook, sp := range span {
		end := narrationEnd(work.AudioFiles, repBook)
		if end > 0 && sp < end*syncCoverageFloor {
			rep.OK = false
			rep.Issues = append(rep.Issues, DerivationIssue{
				Kind:   "sync_partial",
				Detail: fmt.Sprintf("narration from book %d synced to %.0fs of %.0fs (%.0f%%)", repBook, sp, end, sp/end*100),
			})
		}
	}
	return rep, nil
}

// narrationEnd walks the contiguous start_sec chain beginning at repBook and
// returns the narration's end second (the last file's start_sec+duration). A
// file continues the chain when its start_sec meets the running end; two
// narrations that both begin at 0 separate cleanly because only one file
// continues each chain. Returns 0 when repBook isn't among the work's files.
func narrationEnd(files []db.Book, repBook int64) float64 {
	var cur *db.Book
	for i := range files {
		if files[i].ID == repBook {
			cur = &files[i]
			break
		}
	}
	if cur == nil {
		return 0
	}
	const tol = 2.0
	end := cur.StartSec + cur.Duration
	seen := map[int64]bool{cur.ID: true}
	for {
		var next *db.Book
		for i := range files {
			f := &files[i]
			if seen[f.ID] || f.StartSec <= cur.StartSec {
				continue
			}
			if f.StartSec >= end-tol && f.StartSec <= end+tol {
				next = f
				break
			}
		}
		if next == nil {
			return end
		}
		seen[next.ID] = true
		cur = next
		end = cur.StartSec + cur.Duration
	}
}

// StartupDerivationSweep verifies every work and returns the reports that are
// NOT OK — the standing library-wide check. Run at boot and after any re-derive;
// an empty result is the whole library asserting completeness.
func StartupDerivationSweep(store *db.Store) ([]DerivationReport, error) {
	works, err := store.ListWorks()
	if err != nil {
		return nil, err
	}
	var bad []DerivationReport
	for i := range works {
		full, err := store.GetWork(works[i].ID)
		if err != nil || full == nil {
			continue
		}
		rep, err := VerifyWorkDerivation(store, full)
		if err != nil {
			continue
		}
		if !rep.OK {
			bad = append(bad, rep)
		}
	}
	return bad, nil
}
