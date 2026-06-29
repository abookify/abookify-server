package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/llm"
)

// Spoiler-aware book + chapter summaries (#134). Two LLM-generated, cached
// readouts surfaced in the reader:
//   - a per-CHAPTER summary (2–4 sentences of just that chapter), and
//   - a "story so far" RECAP synthesized from the chapter summaries up to the
//     reader's current position — nothing past it, so opening it can't spoil
//     what the reader hasn't reached.
// Both cache to the `summaries` table; a regenerate (?refresh=1) overwrites.
// Everything is gated on a configured LLM (RAG) key.

// chapterSummaryInputCap bounds how much chapter text we send per summary.
const chapterSummaryInputCap = 24000 // ~6k words

const chapterSummarySystem = "You are a precise literary assistant. Summarize ONLY the chapter text the user gives you, in 2–4 sentences, capturing its key events and developments. Use information from the text alone — do not add outside knowledge, do not speculate about what happens later, and do not reference other chapters. Write plainly, no preamble."

const recapSystem = "You write a spoiler-free \"story so far\" recap. You are given numbered per-chapter summaries that END at the reader's current point. Using ONLY those summaries, write a concise recap (1–3 short paragraphs) of what has happened so far — characters, setting, and the main thread. Never reveal, foreshadow, or speculate about anything beyond the provided summaries. No preamble."

// llmClientOr503 returns the configured chat client, or writes a 503 and
// returns nil when no LLM is set up (every summary route is key-gated).
func (s *Server) llmClientOr503(w http.ResponseWriter) *llm.Client {
	rag := s.RAG()
	if rag == nil || rag.Client() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "summaries need an AI provider — add an API key in Settings",
		})
		return nil
	}
	return rag.Client()
}

// ensureChapterSummary returns a chapter's cached summary, generating + caching
// it on a miss (or when refresh is set). Empty/near-empty chapters return "".
func (s *Server) ensureChapterSummary(client *llm.Client, bookID int64, idx int, refresh bool) (string, error) {
	if !refresh {
		if text, _, ok, err := s.store.GetSummary(bookID, "chapter", idx); err != nil {
			return "", err
		} else if ok {
			return text, nil
		}
	}
	ch, err := s.store.GetChapterContent(bookID, idx)
	if err != nil || ch == nil {
		return "", err
	}
	content := strings.TrimSpace(ch.Content)
	if len(strings.Fields(content)) < 20 {
		return "", nil // too short to summarize (title page / divider)
	}
	if len(content) > chapterSummaryInputCap {
		content = content[:chapterSummaryInputCap]
	}
	user := content
	if ch.Title != "" {
		user = "Chapter: " + ch.Title + "\n\n" + content
	}
	resp, err := client.Complete(llm.CompletionRequest{
		System:      chapterSummarySystem,
		Messages:    []llm.Message{{Role: "user", Content: user}},
		MaxTokens:   220,
		Temperature: 0.3,
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Content)
	if text != "" {
		_ = s.store.SaveSummary(bookID, "chapter", idx, text, resp.Model)
	}
	return text, nil
}

// handleChapterSummary: GET /api/books/{bookId}/chapters/{idx}/summary[?refresh=1]
func (s *Server) handleChapterSummary(w http.ResponseWriter, r *http.Request) {
	client := s.llmClientOr503(w)
	if client == nil {
		return
	}
	bookID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("bookId")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bookId"})
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(r.PathValue("idx")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid chapter idx"})
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	_, _, cached, _ := s.store.GetSummary(bookID, "chapter", idx)
	text, err := s.ensureChapterSummary(client, bookID, idx, refresh)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"book_id": bookID, "chapter_idx": idx, "summary": text,
		"cached": cached && !refresh, "model": client.Model(),
	})
}

// handleBookRecap: GET /api/books/{bookId}/recap?up_to={N}[&refresh=1]
// Spoiler-free recap of the story through chapter N (inclusive), synthesized
// from the chapter summaries up to N.
func (s *Server) handleBookRecap(w http.ResponseWriter, r *http.Request) {
	client := s.llmClientOr503(w)
	if client == nil {
		return
	}
	bookID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("bookId")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bookId"})
		return
	}
	upTo, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("up_to")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid up_to"})
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"

	if !refresh {
		if text, _, ok, _ := s.store.GetSummary(bookID, "recap", upTo); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"book_id": bookID, "up_to": upTo, "recap": text, "cached": true, "model": client.Model(),
			})
			return
		}
	}

	// Gather the chapter summaries for every content chapter with index ≤ upTo,
	// generating any that are missing. This is what bounds the recap to the
	// reader's current point — no later chapter is ever consulted.
	chapters, err := s.store.ListChapters(bookID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	sort.Slice(chapters, func(i, j int) bool { return chapters[i].Index < chapters[j].Index })
	var parts []string
	n := 0
	for _, ch := range chapters {
		if ch.Index > upTo {
			break
		}
		sum, err := s.ensureChapterSummary(client, bookID, ch.Index, false)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(sum) == "" {
			continue
		}
		n++
		label := ch.Title
		if label == "" {
			label = fmt.Sprintf("Chapter %d", n)
		}
		parts = append(parts, fmt.Sprintf("%d. %s: %s", n, label, sum))
	}
	if len(parts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"book_id": bookID, "up_to": upTo, "recap": "", "cached": false, "model": client.Model(),
		})
		return
	}

	resp, err := client.Complete(llm.CompletionRequest{
		System:      recapSystem,
		Messages:    []llm.Message{{Role: "user", Content: strings.Join(parts, "\n")}},
		MaxTokens:   400,
		Temperature: 0.3,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	recap := strings.TrimSpace(resp.Content)
	if recap != "" {
		_ = s.store.SaveSummary(bookID, "recap", upTo, recap, resp.Model)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"book_id": bookID, "up_to": upTo, "recap": recap, "cached": false, "model": resp.Model,
	})
}

// invalidateSummaries drops a book's cached summaries when its content changes.
// Best-effort (logged by the caller's flow, not fatal).
func (s *Server) invalidateSummaries(bookID int64) { _ = s.store.DeleteSummariesForBook(bookID) }
