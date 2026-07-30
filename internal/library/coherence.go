package library

import (
	"fmt"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// End-to-end coherence check for a repaired book.
//
// A repair re-transcribes a book and rewrites its text. The reader and within-
// book search both read chapters.content directly, so they move together — but
// every DERIVED artifact is a separate copy that has to be regenerated, and a
// repair that lands with only some of them rebuilt leaves the product disagreeing
// with itself: the reader shows the corrected passage while Q&A cites the invented
// one from a stale chunk at the very same moment. That happened three separate
// times in one day, so "the transcript changed" is not proof the book is coherent;
// this is.
//
// The check is CONTENT-BASED and path-agnostic: it never asks "did the repair run
// step X", it asks "does each surface's text actually match the reader's text right
// now". chapters.content is the source of truth (what the reader + search show);
// every derived surface is compared back to it:
//
//   - Q&A chunks — re-derived exactly from the chapter's current words (a chunk is
//     by construction words[start:end] joined by spaces, so a mismatch is proof the
//     chunk describes text the reader no longer shows). This is the loud one, and
//     the sound one: chunks share the reader's exact Fields(content) basis.
//   - sync_data — the word-timing stream the karaoke reads; its length must track
//     the transcript's current word count (a repaired transcript that grew/shrank
//     with stale sync highlights the wrong words). Reconciled against ALL narrated
//     editions so a coexisting TTS edition isn't misread as a mismatch.
//   - text_trust — the persisted verdict was computed over a word count; if no
//     current transcript edition matches it, the verdict describes an old version.
//   - embeddings — a rebuild resets them to null, so partial coverage means Q&A's
//     vector search silently sees only part of the book (degraded, not a lie).
//
// Paragraphs are deliberately NOT compared: their text normalization diverges from
// the chapter plaintext independently of any repair (case/punctuation/hyphenation),
// so a token comparison is noise — and the one paragraph fault that changes what the
// reader reads also moves the chunks, which are checked exactly. Reader text and
// within-book search both read chapters.content directly, so they move together and
// need no separate check; the derived surfaces above are where a repair can lag.
//
// Any INCOHERENT issue means two surfaces disagree about the text — the failure,
// and it is meant to be loud. DEGRADED issues (missing/partial embeddings, an
// un-chunked book) weaken a surface without making it cite wrong text.

const (
	coherenceSeverityIncoherent = "incoherent" // two surfaces disagree about the text — the failure
	coherenceSeverityDegraded   = "degraded"   // a surface is weakened but not lying

	// A derived word-count may differ from the reader's by a hair (chapter titles,
	// tokenizer edges) without being a different VERSION. These tolerances keep
	// those edges from reading as a repair miss; a real stale artifact diverges far
	// past them (a re-transcription that recovered lost narration, an old
	// segmentation's sync, a trust verdict from a shorter draft).
	syncWordCountTolerance  = 0.05 // sync stream vs transcript words
	trustWordCountTolerance = 0.05 // trust verdict's total_words vs transcript words
)

// CoherenceIssue is one surface disagreeing with (or lagging) the reader text.
type CoherenceIssue struct {
	Surface  string `json:"surface"`           // qa_chunks | paragraphs | sync | text_trust | embeddings
	Severity string `json:"severity"`          // incoherent | degraded
	BookID   int64  `json:"book_id,omitempty"` // 0 for work-level surfaces
	Chapter  int    `json:"chapter"`           // -1 for book/work-level
	Detail   string `json:"detail"`
}

// WorkCoherence is the per-work verdict: coherent iff no surface disagrees.
type WorkCoherence struct {
	WorkID   int64            `json:"work_id"`
	Title    string           `json:"title"`
	Coherent bool             `json:"coherent"` // false if ANY incoherent issue
	Degraded bool             `json:"degraded"` // true if any degraded (but not incoherent) issue
	Issues   []CoherenceIssue `json:"issues"`
}

// CheckWorkCoherence cross-checks every derived surface of a work against the
// reader's text. A read-only diagnostic — it never repairs, it reports.
func CheckWorkCoherence(store *db.Store, workID int64) (*WorkCoherence, error) {
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		return nil, err
	}
	out := &WorkCoherence{WorkID: workID, Title: w.Title, Coherent: true}
	add := func(iss CoherenceIssue) {
		out.Issues = append(out.Issues, iss)
		switch iss.Severity {
		case coherenceSeverityIncoherent:
			out.Coherent = false
		case coherenceSeverityDegraded:
			out.Degraded = true
		}
	}

	// All transcript editions and their current word counts. A work can carry more
	// than one narrated edition (a LibriVox recording AND a generated TTS edition,
	// each with its own transcript + sync) — so sync/trust must reconcile against
	// ALL of them, not the first, or a coexisting second edition reads as a 2× sync
	// mismatch (The Call of the Wild) when it's simply the multi-edition feature.
	var transcriptBookID int64
	transWordCounts := map[int64]int{}
	totalTransWords := 0
	for i := range w.TextFiles {
		b := &w.TextFiles[i]
		if b.Origin == "whisper_transcript" || b.Format == "transcript" {
			if transcriptBookID == 0 {
				transcriptBookID = b.ID
			}
			n := bookWordCount(store, b.ID)
			transWordCounts[b.ID] = n
			totalTransWords += n
		}
	}

	// Per displayed text source: chunks (Q&A) + paragraphs vs the reader text.
	for i := range w.TextFiles {
		b := &w.TextFiles[i]
		if b.Visibility == "internal" {
			continue
		}
		chapters, err := store.ListChapters(b.ID)
		if err != nil {
			continue
		}
		chunks, _ := store.ListChunks(b.ID)
		chunksByCh := map[int][]db.Chunk{}
		for _, c := range chunks {
			chunksByCh[c.ChapterIdx] = append(chunksByCh[c.ChapterIdx], c)
		}
		totalChunks, embedded := 0, 0
		contentChapters := 0

		for _, chMeta := range chapters {
			if chMeta.WordCount < 1 {
				continue
			}
			contentChapters++
			full, err := store.GetChapterContent(b.ID, chMeta.Index)
			if err != nil || full == nil {
				continue
			}
			words := strings.Fields(full.Content)

			// Q&A chunks — exact re-derivation. One issue per chapter (the first
			// stale chunk is enough to prove the chapter's Q&A text is wrong).
			for _, c := range chunksByCh[chMeta.Index] {
				totalChunks++
				if len(c.Embedding) > 0 {
					embedded++
				}
			}
			if s, why := firstStaleChunk(chunksByCh[chMeta.Index], words); s {
				add(CoherenceIssue{
					Surface: "qa_chunks", Severity: coherenceSeverityIncoherent,
					BookID: b.ID, Chapter: chMeta.Index,
					Detail: "Q&A/search chunk " + why + " — Q&A can cite text the reader no longer shows",
				})
			}
		}
		// NOTE: paragraphs are intentionally NOT compared here. Paragraph text is a
		// display artifact whose normalization (case, punctuation, hyphenation)
		// diverges from the chapter plaintext independently of any repair — a
		// token-level comparison flagged a third of the library on "The"/"THE",
		// "Ten"/"Ten-foot", "Frankenstein"/"Frankenstein;" — noise, not incoherence.
		// The one paragraph fault that would actually change what the reader reads
		// (a repaired chapter whose text moved) also moves the chunks, which ARE
		// checked exactly (chunks share the Fields(content) basis, no normalization
		// gap). So chunks are the sound Q&A/search-vector signal; paragraphs aren't.

		if contentChapters > 0 && totalChunks == 0 {
			add(CoherenceIssue{
				Surface: "qa_chunks", Severity: coherenceSeverityDegraded, BookID: b.ID, Chapter: -1,
				Detail: "book has content chapters but no chunks — Q&A has no passages to cite for it",
			})
		} else if embedded > 0 && embedded < totalChunks {
			add(CoherenceIssue{
				Surface: "embeddings", Severity: coherenceSeverityDegraded, BookID: b.ID, Chapter: -1,
				Detail: fmt.Sprintf("%d of %d chunks embedded — Q&A vector search sees only part of the book (rebuild pending)", embedded, totalChunks),
			})
		}
	}

	// The sync stream (word timing) — loaded once; feeds both the sync check and the
	// text-trust freshness check, which share the sidecar word basis.
	syncWords, _ := LoadWorkSyncWords(store, workID)

	// Sync stream vs the transcript text (work-level, INCOHERENT — karaoke lands on
	// the wrong words). The sync stream spans every narrated edition, so it
	// reconciles against the SUM of all transcript editions' words — a repaired
	// transcript that grew/shrank with stale sync diverges far past the tolerance;
	// a coexisting second edition does not.
	if totalTransWords > 0 {
		switch {
		case len(syncWords) == 0:
			add(CoherenceIssue{
				Surface: "sync", Severity: coherenceSeverityIncoherent, BookID: transcriptBookID, Chapter: -1,
				Detail: "transcript has text but no word-timing — karaoke is dead against a readable transcript",
			})
		case countDivergesBeyond(len(syncWords), totalTransWords, syncWordCountTolerance):
			add(CoherenceIssue{
				Surface: "sync", Severity: coherenceSeverityIncoherent, BookID: transcriptBookID, Chapter: -1,
				Detail: fmt.Sprintf("transcript is %d words but the sync stream is %d — sync was not regenerated for the current transcript", totalTransWords, len(syncWords)),
			})
		}
	}

	// Text-trust verdict freshness (work-level, DEGRADED — it's a badge, not the
	// reader/Q&A text). The verdict's word count comes from the sidecar; compare it
	// against the sync stream, which shares that sidecar basis (comparing against the
	// chapter-content count instead cross-basis-mismatches by a few percent even when
	// fresh). If it diverges, the badge predates the latest repair — its sidecar
	// hasn't caught up with the imported transcript, so it can't be healed here (a
	// re-check reads the same stale sidecar); it refreshes when transcription
	// re-writes the sidecar. Soft, because a slightly-old suspect % isn't the product
	// showing the reader different words — that's what the chunk + sync checks catch.
	if len(syncWords) > 0 {
		if row, _ := store.GetTextTrust(workID); row != nil && row.TotalWords > 0 &&
			countDivergesBeyond(row.TotalWords, len(syncWords), trustWordCountTolerance) {
			add(CoherenceIssue{
				Surface: "text_trust", Severity: coherenceSeverityDegraded, BookID: transcriptBookID, Chapter: -1,
				Detail: fmt.Sprintf("trust verdict covers %d words but the current narration has %d — the trust badge predates the latest transcript (sidecar not yet caught up)", row.TotalWords, len(syncWords)),
			})
		}
	}

	return out, nil
}

// CheckLibraryCoherence runs the check across every work, worst first.
func CheckLibraryCoherence(store *db.Store) ([]WorkCoherence, error) {
	works, err := store.ListWorks()
	if err != nil {
		return nil, err
	}
	out := make([]WorkCoherence, 0, len(works))
	for i := range works {
		wc, err := CheckWorkCoherence(store, works[i].ID)
		if err != nil || wc == nil {
			continue
		}
		out = append(out, *wc)
	}
	return out, nil
}

// firstStaleChunk re-derives each chunk from the chapter's current words. A chunk
// is by construction words[StartWord:EndWord] joined by single spaces, so an exact
// mismatch is proof it describes different text — not a heuristic. Returns the
// reason for the first stale chunk (matches chunker.chunksStale's derivation).
func firstStaleChunk(chunks []db.Chunk, words []string) (bool, string) {
	for _, c := range chunks {
		if c.StartWord < 0 || c.EndWord > len(words) || c.StartWord > c.EndWord {
			return true, "spans past the chapter's current words (the chapter shrank under it)"
		}
		if c.Content != strings.Join(words[c.StartWord:c.EndWord], " ") {
			return true, "covers a different wording than the chapter now has (the text was rewritten)"
		}
	}
	return false, ""
}

// bookWordCount sums the current words across a book's chapters (Fields of the
// content — the same basis chunks/paragraphs index into).
func bookWordCount(store *db.Store, bookID int64) int {
	chs, err := store.ListChapters(bookID)
	if err != nil {
		return 0
	}
	total := 0
	for _, ch := range chs {
		full, err := store.GetChapterContent(bookID, ch.Index)
		if err != nil || full == nil {
			continue
		}
		total += len(strings.Fields(full.Content))
	}
	return total
}

// countDivergesBeyond reports whether two word counts differ by more than a
// fraction of the reference (the current transcript).
func countDivergesBeyond(got, reference int, tol float64) bool {
	if reference <= 0 {
		return false
	}
	diff := got - reference
	if diff < 0 {
		diff = -diff
	}
	return float64(diff)/float64(reference) > tol
}
