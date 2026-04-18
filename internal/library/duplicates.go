// Duplicate/variant work detection. Identifies works that are likely the
// same book in different editions or formats (e.g. LibriVox audio of
// Frankenstein + Gutenberg EPUB of Frankenstein sitting as separate works
// because the matcher didn't auto-pair them).
//
// Proactive surfacing helps users confirm-and-merge rather than letting
// duplicates quietly clutter the library.
package library

import (
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// DuplicateGroup is a set of works that look like the same title.
// The first work is the "target" (most complete) — the UI suggests
// merging others into it.
type DuplicateGroup struct {
	NormalizedKey string    `json:"normalized_key"` // e.g. "frankenstein-shelley"
	Works         []db.Work `json:"works"`
}

// FindDuplicateWorks returns groups of works that share a normalized
// (title, author) key. Groups of size 1 are excluded — no duplicates there.
//
// Normalization: lowercase, strip punctuation, collapse whitespace, drop
// common trailing noise ("(unabridged)", "[ebook]", "audiobook", etc.).
func FindDuplicateWorks(store *db.Store) ([]DuplicateGroup, error) {
	works, err := store.ListWorks()
	if err != nil {
		return nil, err
	}

	groups := map[string][]db.Work{}
	for _, w := range works {
		key := normalizeWorkKey(w.Title, w.Author)
		if key == "" {
			continue
		}
		groups[key] = append(groups[key], w)
	}

	var result []DuplicateGroup
	for key, ws := range groups {
		if len(ws) < 2 {
			continue
		}
		// Sort within group: most "complete" first (has both audio + text
		// beats audio-only beats text-only; more files beats fewer).
		sort.SliceStable(ws, func(i, j int) bool {
			return completeness(ws[i]) > completeness(ws[j])
		})
		result = append(result, DuplicateGroup{NormalizedKey: key, Works: ws})
	}

	// Sort groups by key for stable output.
	sort.Slice(result, func(i, j int) bool {
		return result[i].NormalizedKey < result[j].NormalizedKey
	})
	return result, nil
}

// completeness is a rough "how useful is this work" score for picking the
// merge target. Higher = more complete.
func completeness(w db.Work) int {
	score := 0
	if w.HasAudio {
		score += 100
	}
	if w.HasText {
		score += 100
	}
	score += len(w.AudioFiles) + len(w.TextFiles)
	return score
}

// normalizeWorkKey produces a dedup key from title + author. Returns ""
// if both are empty (can't dedup unnamed works).
func normalizeWorkKey(title, author string) string {
	t := normalizeTitle(title)
	a := normalizeAuthor(author)
	if t == "" && a == "" {
		return ""
	}
	if a == "" {
		return t
	}
	return t + "-" + a
}

// normalizeTitle lowercases, strips punctuation, and removes common
// edition/format noise words that often differ between duplicates.
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Drop parenthetical/bracket noise.
	for _, open := range []string{"(", "[", "{"} {
		if idx := strings.Index(s, open); idx >= 0 {
			s = s[:idx]
		}
	}
	// Drop common suffixes.
	noise := []string{
		" unabridged", " audiobook", " ebook", " (transcript)",
		" volume 1", " volume 2", " vol 1", " vol 2",
		" [epub]", " [mobi]", " hd", " remastered",
	}
	for _, n := range noise {
		s = strings.ReplaceAll(s, n, "")
	}
	// Remove leading "the " / "a " / "an ".
	for _, prefix := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(s, prefix) {
			s = s[len(prefix):]
		}
	}
	// Collapse punctuation and whitespace.
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace && b.Len() > 0 {
			b.WriteByte('-')
			prevSpace = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// normalizeAuthor is less aggressive than title — preserves name structure.
// Converts "Mary Wollstonecraft Shelley" and "Shelley, Mary" to
// a common form using the last non-empty word as a surname anchor.
func normalizeAuthor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// Handle "Last, First" by flipping to "First Last".
	if idx := strings.Index(s, ","); idx >= 0 {
		last := strings.TrimSpace(s[:idx])
		rest := strings.TrimSpace(s[idx+1:])
		s = rest + " " + last
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	// Use the last word (typically the surname) as the key anchor.
	last := fields[len(fields)-1]
	// Strip trailing punctuation.
	last = strings.TrimRight(last, ".,;:")
	return last
}
