package library

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Spoken chapter titles come from the narrator's announcement, cut at the first
// sentence boundary or the first real pause. Whisper sometimes stretches the
// body's first word backwards over that pause ("Battle of the Generations Let
// [1.5 s] us begin", "Nice Guys Finish First Nice [1.7 s] guys finish last"),
// so the cut falls one word late and the TOC carries a stray trailing word —
// on The Selfish Gene, two of thirteen. Those rows are the canonical TOC every
// surface renders (canon.active.chapters_anchor_book_id), so the junk is what
// PJ sees on his phone.
//
// Once a publisher edition is aligned to the narration, the publisher's title
// is the authority (the same order as chapter authority: embedded > detected).
// This trims a spoken title back to the publisher's when — and only when — the
// spoken words START with the publisher's words and carry at most two extra:
// agreement on the leading words is what makes the trailing ones bleed rather
// than a different title. The spoken casing is kept; only the extra words go.
// Titles that already agree, or differ in substance, are left alone.

// spokenTitleRe: "Chapter 8: Battle of the Generations Let" (sidecar path) or
// "8. Battle of the Generations Let" (transcript_split path).
var spokenTitleRe = regexp.MustCompile(`^\s*(?:(Chapter\s+\d+)\s*:\s*|(\d{1,3})\.\s+)(.+?)\s*$`)

// ebookTitlePrefixRe strips the number from a publisher title: "8. Battle of
// the generations", "Chapter 8: Battle…", "CHAPTER VIII. Battle…".
var ebookTitlePrefixRe = regexp.MustCompile(`(?i)^\s*(?:chapter\s+(?:\d+|[ivxlc]+)|\d{1,3})\s*[.:\-–—]?\s*`)

const maxBleedWords = 2

// ReconcileSpokenTitles trims bled-in trailing words from the spoken chapter
// titles of every narration and transcript book on the work, using the
// publisher ebook's titles as the authority. Returns the number of titles
// changed.
func ReconcileSpokenTitles(store *db.Store, work *db.Work, ebookID int64) (int, error) {
	ebookChs, err := store.ListChapters(ebookID)
	if err != nil {
		return 0, err
	}
	pubByNum := map[int]string{}
	for _, ch := range ebookChs {
		n := extractChapterNum(ch.Title)
		if n <= 0 {
			continue
		}
		sub := strings.TrimSpace(ebookTitlePrefixRe.ReplaceAllString(ch.Title, ""))
		if sub == "" {
			continue
		}
		if _, dup := pubByNum[n]; dup {
			pubByNum[n] = "" // ambiguous numbering (parts/books) — never trim on it
			continue
		}
		pubByNum[n] = sub
	}
	if len(pubByNum) == 0 {
		return 0, nil
	}

	var targets []int64
	for _, b := range work.AudioFiles {
		targets = append(targets, b.ID)
	}
	for _, b := range work.TextFiles {
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			targets = append(targets, b.ID)
		}
	}
	changed := 0
	for _, bookID := range targets {
		chs, err := store.ListChapters(bookID)
		if err != nil {
			return changed, err
		}
		for _, ch := range chs {
			fixed, ok := trimBleedAgainst(ch.Title, pubByNum)
			if !ok {
				continue
			}
			if err := store.UpdateChapterTitle(bookID, ch.Index, fixed); err != nil {
				return changed, fmt.Errorf("update chapter %d/%d title: %w", bookID, ch.Index, err)
			}
			log.Printf("reconciled spoken title on book %d ch %d: %q → %q (publisher edition %d)", bookID, ch.Index, ch.Title, fixed, ebookID)
			changed++
		}
	}
	return changed, nil
}

// trimBleedAgainst returns the trimmed title and true when spoken carries the
// publisher's title for its number plus 1..maxBleedWords trailing words.
func trimBleedAgainst(spoken string, pubByNum map[int]string) (string, bool) {
	m := spokenTitleRe.FindStringSubmatch(spoken)
	if m == nil {
		return "", false
	}
	n := extractChapterNum(spoken)
	pub, ok := pubByNum[n]
	if !ok || pub == "" {
		return "", false
	}
	spokenWords := strings.Fields(m[3])
	pubWords := strings.Fields(normalize(pub))
	extra := len(spokenWords) - len(pubWords)
	if extra < 1 || extra > maxBleedWords || len(pubWords) == 0 {
		return "", false
	}
	if normalize(strings.Join(spokenWords[:len(pubWords)], " ")) != strings.Join(pubWords, " ") {
		return "", false
	}
	kept := strings.Join(spokenWords[:len(pubWords)], " ")
	if m[1] != "" {
		return m[1] + ": " + kept, true
	}
	return m[2] + ". " + kept, true
}
