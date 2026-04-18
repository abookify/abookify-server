// Display source resolver. Given a work, picks the highest-authority visible
// source to show in the reader (text) or play (audio). Consumers include the
// web UI, mobile API, and Q&A retrieval — all of which currently hard-pick the
// first available source without considering authority.
//
// Resolution order:
//   1. User override per work (if persisted in settings — future)
//   2. Highest OriginAuthority among visible sources of the requested media type
//   3. Nil if no qualifying source exists
package library

import (
	"github.com/pj/abookify/internal/db"
)

// ResolveDisplayText returns the highest-authority visible text source for a
// work, or nil if none exists. Prefers publisher EPUB > MOBI > PDF > user
// upload > transcript. Pipeline intermediates (visibility="internal") are
// excluded.
func ResolveDisplayText(work *db.Work) *db.Book {
	return resolveByAuthority(work.TextFiles)
}

// ResolveDisplayAudio returns the highest-authority visible audio source for a
// work. Prefers author recordings > narrator > librivox > TTS-generated.
func ResolveDisplayAudio(work *db.Work) *db.Book {
	return resolveByAuthority(work.AudioFiles)
}

func resolveByAuthority(books []db.Book) *db.Book {
	var best *db.Book
	bestScore := -1
	for i := range books {
		b := &books[i]
		if b.Visibility == "internal" {
			continue
		}
		score := db.OriginAuthority(b.Origin)
		if score > bestScore {
			bestScore = score
			best = b
		}
	}
	return best
}

// ResolveAlignmentTarget returns the best text book to use as the "from" side
// of a forced alignment query — i.e. the source whose prose should be shown
// to the user with audio timestamps composed through the alignment chain.
//
// Typically this is the highest-authority ebook. Falls back to transcript if
// no ebook exists (audio-only works).
func ResolveAlignmentTarget(work *db.Work) *db.Book {
	// Prefer publisher sources, then fall back to any visible text.
	for _, origin := range []string{"publisher_epub", "publisher_mobi", "publisher_pdf"} {
		for i := range work.TextFiles {
			if work.TextFiles[i].Origin == origin && work.TextFiles[i].Visibility != "internal" {
				return &work.TextFiles[i]
			}
		}
	}
	return ResolveDisplayText(work)
}
