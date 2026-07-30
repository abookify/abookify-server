// Multi-turn chat over a work. Wraps the single-shot AskWithCitations
// pipeline (vector search → context → LLM) but threads prior turns into
// the LLM call so follow-ups make sense.
package library

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/llm"
)

// AskInSession runs the work-scoped Q&A pipeline with conversation history.
// The history is the prior messages in this chat session (oldest → newest);
// the new question gets appended internally. Citations are computed for the
// most recent retrieval pass only — historical messages don't get re-cited.
//
// scope mirrors AskWithCitations — pass the zero value for whole-work,
// or constrain to a chapter/up-to-here/paragraph.
//
// Returns the assistant's reply augmented with citations. Caller is
// responsible for persisting the user message + this reply to qa_messages.
// extractOnly (#130 belt-and-braces): answer ONLY from the book's own text —
// return the retrieved, position-bounded passages verbatim and never let the
// model generate. Sidesteps the memorized-classic leak entirely (the retrieval
// bound already caps citations at the reader's position; extract-only makes the
// ANSWER itself be those passages, so there's nothing for the model to leak).
// Works even with no LLM key (keyword retrieval), so it's fully hermetic.
func AskInSession(store *db.Store, rag *llm.RAG, workID int64, history []db.QAMessage, question string, scope QueryScope, extractOnly bool) (*llm.Answer, error) {
	if !extractOnly && (rag == nil || rag.Client() == nil) {
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

	retrieved, err := retrievePassages(store, rag, work, target, question, scope)
	if err != nil {
		return nil, err
	}

	// Chapter-reference boost: if the question explicitly names a
	// chapter ("summarize chapter 26"), force-include all chunks for
	// that chapter alongside the vector hits. Pure semantic similarity
	// often misses named chapters because the question's wording
	// doesn't resemble the chapter's prose. Dedup against vector hits
	// so we don't repeat chunks. Skip when the user already pinned
	// scope to a single chapter — re-adding it is redundant.
	chapters, _ := store.ListChapters(target.ID)
	if scope.Type != "chapter" && scope.Type != "paragraph" {
		if refs := ParseChapterRefs(question, chapters); len(refs) > 0 {
			boost, _ := FetchChapterChunks(store, target.ID, refs)
			boost = scope.FilterChunks(store, boost)
			seen := map[int64]bool{}
			for _, c := range retrieved {
				seen[c.ID] = true
			}
			for _, c := range boost {
				if !seen[c.ID] {
					retrieved = append(retrieved, c)
					seen[c.ID] = true
				}
			}
		}
	}

	// Not-started case: a FRESH chat (no history) asking within a reading scope
	// pinned at/before the very start, with nothing retrieved, means the reader
	// hasn't read anything yet. Say so plainly + usefully — NOT the generic "hasn't
	// come up yet" decline (which reads as "topic not found") and NOT a model guess.
	// Every new user hits this on their first curious "recap so far" tap. Follow-ups
	// (history present) and mid-book queries still flow to the model below.
	if len(retrieved) == 0 && len(history) == 0 && scope.Type == "up_to_chapter" && scope.ChapterIdx <= 0 {
		return &llm.Answer{Text: NotStartedRecapMessage, Model: "no-passages", Chunks: 0}, nil
	}

	// Even with no retrieval, we still let the model respond — it can use
	// the prior context to answer follow-ups like "summarize the above".
	titleCache := map[int64]map[int]string{}
	getTitle := func(bookID int64, ch int) string {
		m, ok := titleCache[bookID]
		if !ok {
			m = map[int]string{}
			chapters, _ := store.ListChapters(bookID)
			for _, c := range chapters {
				m[c.Index] = c.Title
			}
			titleCache[bookID] = m
		}
		if t, ok := m[ch]; ok && t != "" {
			return t
		}
		return fmt.Sprintf("Chapter %d", ch+1)
	}

	ac := newAlignmentContext(store, workID)

	var contextBuf strings.Builder
	var citations []llm.Citation
	for i, chunk := range retrieved {
		chTitle := getTitle(chunk.BookID, chunk.ChapterIdx)
		contextBuf.WriteString(fmt.Sprintf("[Passage %d - %s]\n", i+1, chTitle))
		contextBuf.WriteString(chunk.Content)
		contextBuf.WriteString("\n\n")

		excerpt := chunk.Content
		if len(excerpt) > 150 {
			excerpt = excerpt[:150] + "..."
		}
		cit := llm.Citation{
			BookID:       chunk.BookID,
			ChapterIdx:   chunk.ChapterIdx,
			ChapterTitle: chTitle,
			StartWord:    chunk.StartWord,
			EndWord:      chunk.EndWord,
			Excerpt:      excerpt,
		}
		if abkID, startSec, endSec, ok := ac.audioTimesFor(chunk); ok {
			cit.AudioStartSec = startSec
			cit.AudioEndSec = endSec
			cit.AudioBookID = abkID
		}
		citations = append(citations, cit)
	}

	// Extract-only: hand back the passages themselves, no model generation.
	if extractOnly {
		return composeExtractAnswer(retrieved, citations, getTitle), nil
	}

	// Shared citation guidance for both prompts.
	citationStyle := `Citation style: NEVER mention "Passage N", "passages 3-5", or any reference to internal passage numbers — the reader sees your prose plus a separate Sources panel that names the chapters. Cite by chapter name or a short inline quote (e.g., 'In Chapter 5, the narrator describes…'). The passage-N labels in your context are an internal hint for you only.`

	var systemPrompt string
	if scope.isWholeBook() {
		systemPrompt = fmt.Sprintf(`You are a knowledgeable literary assistant helping a reader understand "%s".
Answer questions based on the provided passages and the prior conversation.

%s

If the passages don't contain enough information to answer, say so honestly.
Keep answers concise but thorough — 2-4 paragraphs.`, work.Title, citationStyle)
	} else {
		// Bounded (spoiler-safe) scope — the reader is somewhere mid-book. Mirror the
		// strict single-shot guard: OMIT the book title (its name alone primes a model
		// that has the book memorised), forbid outside knowledge, and require an EXACT
		// decline when the passages don't contain the answer — so a memorised classic
		// can't leak plot the reader hasn't reached, even in generated mode. (#130)
		systemPrompt = fmt.Sprintf(`You answer a reader's question using ONLY the passages provided and the earlier conversation — the only part of the book they have read so far. You do NOT know this book; ignore anything you might recall about it from elsewhere and rely solely on these passages.

Hard rules:
- Use ONLY facts explicitly stated in the passages (or established earlier in this conversation). No outside knowledge, no guessing, no recognising the book.
- If the passages do not clearly contain the answer, reply with EXACTLY this and nothing more: "That hasn't come up yet in what you've read."
- Never name or describe a character, event, death, twist, or ending that is not present in the passages — that would spoil the story for the reader.

%s`, citationStyle)
	}

	// Build the message list: prior turns verbatim, then a final user
	// turn containing both the new passages and the new question.
	messages := make([]llm.Message, 0, len(history)+1)
	for _, m := range history {
		// Map our roles directly to the LLM provider's roles.
		messages = append(messages, llm.Message{Role: m.Role, Content: m.Content})
	}
	if contextBuf.Len() > 0 {
		messages = append(messages, llm.Message{
			Role: "user",
			Content: fmt.Sprintf("Here are relevant passages from the book:\n\n%s\nQuestion: %s",
				contextBuf.String(), question),
		})
	} else {
		messages = append(messages, llm.Message{Role: "user", Content: question})
	}

	resp, err := rag.Client().Complete(llm.CompletionRequest{
		System:      systemPrompt,
		Messages:    messages,
		MaxTokens:   1024,
		Temperature: 0.3,
	})
	if err != nil {
		return nil, fmt.Errorf("llm completion: %w", err)
	}

	return &llm.Answer{
		Text:      resp.Content,
		Citations: citations,
		Model:     resp.Model,
		Chunks:    len(retrieved),
	}, nil
}

// composeExtractAnswer builds a book-text-only answer from the ALREADY-retrieved,
// position-bounded passages — verbatim, grouped by chapter, with NO model
// generation. Because the text is exactly the retrieved passages (which the
// scope has already capped at the reader's position), it cannot surface anything
// ahead of where they are — the whole point of extract-only mode. Pure function
// of its inputs, so it's directly testable.
func composeExtractAnswer(retrieved []db.Chunk, citations []llm.Citation, getTitle func(int64, int) string) *llm.Answer {
	if len(retrieved) == 0 {
		return &llm.Answer{
			Text:   "That hasn't come up yet in what you've read.",
			Model:  "extract-only",
			Chunks: 0,
		}
	}
	var b strings.Builder
	b.WriteString("Straight from the book, up to where you're reading:\n\n")
	lastKey := ""
	for _, c := range retrieved {
		key := fmt.Sprintf("%d:%d", c.BookID, c.ChapterIdx)
		if key != lastKey {
			if lastKey != "" {
				b.WriteString("\n")
			}
			b.WriteString("**" + getTitle(c.BookID, c.ChapterIdx) + "**\n")
			lastKey = key
		}
		b.WriteString(strings.TrimSpace(c.Content))
		b.WriteString("\n\n")
	}
	return &llm.Answer{
		Text:      strings.TrimRight(b.String(), "\n"),
		Citations: citations,
		Model:     "extract-only",
		Chunks:    len(retrieved),
	}
}

// DeriveSessionTitle produces a short, human-friendly title for a chat
// session based on its first user message. Used when the session was
// auto-created (title still "New chat") and we want the sidebar to show
// something meaningful.
func DeriveSessionTitle(firstMessage string) string {
	t := strings.TrimSpace(firstMessage)
	t = strings.ReplaceAll(t, "\n", " ")
	t = strings.ReplaceAll(t, "\t", " ")
	for strings.Contains(t, "  ") {
		t = strings.ReplaceAll(t, "  ", " ")
	}
	const maxLen = 60
	if len(t) > maxLen {
		t = strings.TrimSpace(t[:maxLen]) + "…"
	}
	if t == "" {
		return "New chat"
	}
	return t
}

// MarshalCitations is a small helper so handlers don't need to import
// encoding/json + the llm package together for the storage roundtrip.
func MarshalCitations(c []llm.Citation) string {
	if len(c) == 0 {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// UnmarshalCitations decodes the stored citations_json column. Returns
// nil on empty input or parse failure (caller treats as "no citations").
func UnmarshalCitations(s string) []llm.Citation {
	if s == "" {
		return nil
	}
	var out []llm.Citation
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
