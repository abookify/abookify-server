package library

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// The narration's detected chapters are the canonical TOC every surface renders
// (canon.active.chapters_anchor_book_id — mobile's list, ⏭⏮, header). Their
// titles are what the narrator SAID, cut at the first pause: "Chapter 2",
// "Chapter 3: 3", "Chapter 14: This is a LibriV…". A LibriVox reader rarely
// speaks the chapter's name, so re-detection can never produce "The Falling
// Star" — the data that has the names is the publisher ebook, and the web
// reader already shows them. The result on every showcase book but one: web
// says "Stave One", the phone says "Chapter 1". Every surface correct, the
// answer wrong — the data is fine and the thing naming it is not.
//
// PropagateEbookTitles copies the publisher edition's chapter names onto the
// narration's chapter rows BY TIME, once the edition is aligned and the chain
// is trusted (the same gate that decides whether the reader shows the ebook
// at all): an anchor chapter takes the title of the ebook chapter whose
// alignment-timed START is nearest the anchor chapter's start — the pairing
// the timing audit proves holds within seconds — allowing the ebook start to
// trail the announcement by up to titleTrailSec (a LibriVox file opens with a
// 25–55 s credit between the spoken "Stave One" and the first line of text)
// or lead it by a few seconds. Not by range containment: a narration that
// announces fewer units than the ebook has (War of the Worlds: 7 spoken, 27
// in the book) makes an anchor chapter span several ebook chapters, and its
// midpoint names the wrong one. Only informative ebook titles propagate —
// "CHAPTER IV." carries no more than the spoken "Chapter 4", so the spoken
// title stays. When two anchor chapters both sit near one ebook start (an
// intro "Prelude" before the first announcement), the nearer one takes the
// name and the other keeps its spoken title.
//
// Titles are overwritten in place: the rows are derived data, re-detection
// regenerates them, and the propagation re-runs at the end of every anchor
// alignment (ComputeAnchorAlignment) — so a re-align keeps the names current.

// bareNumberTitleRe: a title that says no more than the spoken "Chapter N" —
// "Chapter 4", "CHAPTER IV.", "Chapter Six", "IV.", "12". A unit the book
// names differently ("STAVE ONE.", "Letter 3", "Part I") IS information: it
// is the book's own structure, and the web reader already shows it.
var bareNumberTitleRe = regexp.MustCompile(`(?i)^\s*(?:chapter)?\s*(?:\d{1,4}|[ivxlcdm]{1,8}|(?:one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty)(?:[\s-]+(?:one|two|three|four|five|six|seven|eight|nine))?)\s*[.:\-–—]?\s*$`)

var nbspRe = regexp.MustCompile(`[\s\x{00a0}\x{2007}\x{202f}]+`)

// titleLeadSec / titleTrailSec: how far an ebook chapter's timed start may sit
// before / after the spoken announcement and still be the same chapter.
const (
	titleLeadSec  = 20.0
	titleTrailSec = 90.0
)

var lettersOnlyRe = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeChapterTitle collapses every kind of whitespace (Gutenberg titles
// carry non-breaking spaces: "STAVE ONE.") to one space and trims.
func normalizeChapterTitle(t string) string {
	return strings.TrimSpace(nbspRe.ReplaceAllString(t, " "))
}

// informativeEbookTitle reports whether a publisher chapter title names the
// chapter rather than only numbering it (or being front-matter boilerplate).
func informativeEbookTitle(title, workTitle string) bool {
	t := normalizeChapterTitle(title)
	if t == "" || IsBoilerplateChapterTitle(t) || bareNumberTitleRe.MatchString(t) {
		return false
	}
	lt := strings.ToLower(strings.TrimRight(t, ". "))
	switch lt {
	case "contents", "table of contents", "front matter", "title page", "copyright":
		return false
	}
	if workTitle != "" {
		a := lettersOnlyRe.ReplaceAllString(lt, "")
		b := lettersOnlyRe.ReplaceAllString(strings.ToLower(workTitle), "")
		if a != "" && (a == b || strings.HasPrefix(b, a) && len(a) >= 6) {
			return false // the book's own title stamped on a spine file ("D R A C U L A")
		}
	}
	return true
}

// PropagateEbookTitles applies the publisher edition's chapter names to the
// narration + transcript chapter rows of the work by time. dryRun reports the
// changes without writing them. Returns the changes as "book/idx: old → new".
func PropagateEbookTitles(store *db.Store, work *db.Work, ebookID int64, dryRun bool) ([]string, error) {
	// Trust gate: the same chain quality that lets the reader show the ebook.
	wc, err := BuildCoverage(store, work.ID)
	if err != nil {
		return nil, err
	}
	trusted := false
	for _, pc := range wc.Pairs {
		if pc.ByConstruction || pc.Ebook.BookID != ebookID || pc.Unit != "word" {
			continue
		}
		if pc.AudioToEbookInText >= minChainConfidence {
			trusted = true
		}
	}
	if !trusted {
		return nil, nil
	}
	ranges, err := EbookChapterAudioRanges(store, ebookID)
	if err != nil || len(ranges) == 0 {
		return nil, err
	}
	ebookChs, err := store.ListChapters(ebookID)
	if err != nil {
		return nil, err
	}
	type named struct {
		start, end float64
		title      string
	}
	var names []named
	for _, ch := range ebookChs {
		rng, ok := ranges[ch.Index]
		if !ok || rng[1] <= rng[0] || !informativeEbookTitle(ch.Title, work.Title) {
			continue
		}
		names = append(names, named{rng[0], rng[1], normalizeChapterTitle(ch.Title)})
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Slice(names, func(i, j int) bool { return names[i].start < names[j].start })

	var targets []int64
	for _, b := range work.AudioFiles {
		targets = append(targets, b.ID)
	}
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			targets = append(targets, b.ID)
		}
	}
	var changes []string
	for _, bookID := range targets {
		chs, err := store.ListChapters(bookID)
		if err != nil {
			return changes, err
		}
		if len(chs) == 0 {
			continue
		}
		// Which anchor chapter owns each ebook chapter: the anchor whose start
		// is nearest the ebook chapter's timed start, within the lead/trail
		// window; each anchor chapter names at most one ebook chapter.
		// Each narration row takes the ebook chapter that COVERS most of it —
		// not the one whose text starts nearest its first second. Carol's
		// stave01 row (0–2318 s) contains the 17-second Preface and all of
		// Stave One; nearest-start named the whole row "Preface" (card 37).
		// A chapter that already named an earlier row names later rows as
		// "… (continued)": Sherlock's false mid-story boundaries then read
		// as the story they are in, not as "Chapter 3".
		owner := map[int]int{} // names index → chapter index (first row)
		continued := map[int]string{}
		for _, ch := range chs {
			if ch.EndSec <= ch.StartSec {
				continue
			}
			best, bestOv := -1, 0.0
			for k, n := range names {
				lo, hi := n.start, n.end
				if lo < ch.StartSec {
					lo = ch.StartSec
				}
				if hi > ch.EndSec {
					hi = ch.EndSec
				}
				if ov := hi - lo; ov > bestOv {
					best, bestOv = k, ov
				}
			}
			if best < 0 || bestOv < 0.25*(ch.EndSec-ch.StartSec) {
				continue // the row is mostly outside every named chapter
			}
			if _, taken := owner[best]; !taken {
				owner[best] = ch.Index
			} else {
				continued[ch.Index] = names[best].title + " (continued)"
			}
		}
		apply := map[int]string{}
		for k, idx := range owner {
			apply[idx] = names[k].title
		}
		for idx, t := range continued {
			apply[idx] = t
		}
		for idx, title := range apply {
			old := chapterTitle(chs, idx)
			if normalizeChapterTitle(old) == title {
				continue
			}
			changes = append(changes, fmt.Sprintf("book %d ch %d: %q → %q", bookID, idx, old, title))
			if dryRun {
				continue
			}
			if err := store.UpdateChapterTitle(bookID, idx, title); err != nil {
				return changes, fmt.Errorf("update chapter %d/%d title: %w", bookID, idx, err)
			}
			log.Printf("propagated ebook title on book %d ch %d: %q → %q (publisher edition %d)", bookID, idx, old, title, ebookID)
		}
	}
	sort.Strings(changes)
	return changes, nil
}

func chapterStart(chs []db.Chapter, idx int) float64 {
	for _, c := range chs {
		if c.Index == idx {
			return c.StartSec
		}
	}
	return 0
}

func chapterTitle(chs []db.Chapter, idx int) string {
	for _, c := range chs {
		if c.Index == idx {
			return c.Title
		}
	}
	return ""
}
