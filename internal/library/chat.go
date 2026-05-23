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
func AskInSession(store *db.Store, rag *llm.RAG, workID int64, history []db.QAMessage, question string, scope QueryScope) (*llm.Answer, error) {
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

	systemPrompt := fmt.Sprintf(`You are a knowledgeable literary assistant helping a reader understand "%s".
Answer questions based on the provided passages and the prior conversation.

IMPORTANT — citation style: NEVER mention "Passage N", "passages 3-5", or
any reference to internal passage numbers. The user does NOT see passage
numbers; they see your prose answer plus a separate Sources panel below it
that names the chapters. Cite by chapter name or quote a short phrase
inline (e.g., 'In Chapter 5, Norm describes…' or 'as the narrator says,
"…"'). The passage-N labels in your context are an internal hint for you
only.

If the passages don't contain enough information to answer, say so honestly.
Keep answers concise but thorough — 2-4 paragraphs.`, work.Title)

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
