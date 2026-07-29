package library

import (
	"fmt"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// Realtime-voice book grounding. A voice conversation is grounded in the book by
// giving the realtime model a retrieval TOOL it calls with the user's question;
// VoiceContext is what that tool runs server-side. The passages it returns are
// the ONLY book content that reaches the model, so the egress boundary lives
// here — and it is the SAME bound as text Q&A (retrievePassages under the
// resolved scope), enforced in code, not by convention.
//
// Asking a question ALOUD must never leak what typing it would not:
//   - reading-position / scope bound: retrievePassages applies scope.FilterChunks,
//     so a "reading" scope never returns content past the reader's chapter —
//     identical to the chat/ask path (TestVoiceContext_BoundedToReadingPosition).
//   - extract-only: voice is inherently generative (the model speaks), so — like
//     recap + chapter summary — it cannot honour extract-only's absolute
//     no-generation guarantee. Rather than leak, it DECLINES book grounding
//     (Grounded=false); the voice chat still works, just not grounded in the book.

// VoiceContextPassage is one bounded passage handed to the voice model.
type VoiceContextPassage struct {
	ChapterTitle string `json:"chapter_title"`
	Content      string `json:"content"`
}

// VoiceContextResult is the retrieval-tool response for a realtime voice turn.
type VoiceContextResult struct {
	Grounded bool                  `json:"grounded"`
	Reason   string                `json:"reason,omitempty"`
	Passages []VoiceContextPassage `json:"passages,omitempty"`
}

// VoiceContext retrieves the reading-position-bounded passages for a realtime
// voice question, honouring the SAME scope + extract-only rules as text Q&A.
func VoiceContext(store *db.Store, rag *llm.RAG, workID int64, query string, scope QueryScope, extractOnly bool) (*VoiceContextResult, error) {
	// Extract-only: never feed book content to a generative voice model — decline
	// grounding, exactly as recap/chapter-summary decline (see qa.go / #130). This
	// is what stops voice leaking what text Q&A (verbatim passages) does not.
	if extractOnly {
		return &VoiceContextResult{
			Grounded: false,
			Reason:   "spoiler-safe (answer only from the book text) is on, so book questions by voice are off in this mode — a spoken answer is generated and can't be spoiler-guaranteed. Turn it off in Book Q&A settings to discuss the book aloud.",
		}, nil
	}
	if rag == nil || rag.Client() == nil {
		return nil, fmt.Errorf("LLM not configured")
	}
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return nil, fmt.Errorf("work not found")
	}
	target := ResolveAlignmentTarget(work)
	if target == nil {
		return nil, fmt.Errorf("no text content for this work")
	}

	// SAME bounded retrieval as Q&A. scope.FilterChunks (inside) enforces the
	// reading-position bound — passages never exceed the resolved scope.
	retrieved, err := retrievePassages(store, rag, work, target, query, scope)
	if err != nil {
		return nil, err
	}

	res := &VoiceContextResult{Grounded: true}
	for _, chunk := range retrieved {
		res.Passages = append(res.Passages, VoiceContextPassage{
			ChapterTitle: voiceChapterTitle(store, chunk.BookID, chunk.ChapterIdx),
			Content:      chunk.Content,
		})
	}
	return res, nil
}

// voiceChapterTitle resolves a chapter's display title, falling back to
// "Chapter N" (1-based) when untitled — mirrors qa.go's citation titling.
func voiceChapterTitle(store *db.Store, bookID int64, ch int) string {
	chapters, _ := store.ListChapters(bookID)
	for _, c := range chapters {
		if c.Index == ch && c.Title != "" {
			return c.Title
		}
	}
	return fmt.Sprintf("Chapter %d", ch+1)
}
