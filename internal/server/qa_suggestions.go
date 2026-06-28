package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Q&A quick-question starter chips (#131). A few per-book prompts shown above
// the chat input that pre-fill it. Generic ones ("Summarize where I am", "What
// just happened?") are spoiler-safe by construction — they ask about the
// reader's current point, and the answer is bounded by the chat's spoiler scope
// (#130). Entity chips ("Who is X again?") are drawn from the (experimental)
// cast and bounded to characters the reader has ALREADY met — a character whose
// earliest mention falls past the reader's chapter is withheld so the chip text
// itself can't spoil that someone shows up later.

type qaSuggestion struct {
	Label  string `json:"label"`  // short chip text
	Prompt string `json:"prompt"` // what it pre-fills into the input
}

// handleQASuggestions: GET /api/works/{id}/qa-suggestions?book_id=B&chapter=N
func (s *Server) handleQASuggestions(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	bookID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("book_id")), 10, 64)
	chapter, hasChapter := -1, false
	if c := strings.TrimSpace(r.URL.Query().Get("chapter")); c != "" {
		if n, err := strconv.Atoi(c); err == nil {
			chapter, hasChapter = n, true
		}
	}

	out := []qaSuggestion{
		{Label: "Summarize where I am", Prompt: "Summarize what has happened so far, up to where I'm reading."},
		{Label: "What just happened?", Prompt: "What just happened in this chapter?"},
		{Label: "Main characters so far", Prompt: "Who are the main characters introduced so far?"},
	}

	// Entity chips from the cast (experimental; only present when BookNLP has
	// run), bounded to characters the reader has already met.
	out = append(out, s.entitySuggestions(workID, bookID, chapter, hasChapter)...)

	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}

// entitySuggestions builds up to two "Who is X again?" chips for top characters
// the reader has ALREADY met. Spoiler-bounding without relying on BookNLP
// mention offsets (which aren't stored): we load only the text of chapters up
// to the reader's position and check whether the character's name appears in
// it. A character who first shows up later simply isn't in that text, so the
// chip can't reveal them early. Empty when there's no cast or no chapter bound
// (so we never guess past the reader).
func (s *Server) entitySuggestions(workID, bookID int64, chapter int, hasChapter bool) []qaSuggestion {
	if !hasChapter || bookID == 0 {
		return nil
	}
	chars, err := s.store.ListCharactersForWork(workID)
	if err != nil || len(chars) == 0 {
		return nil
	}
	chapters, err := s.store.ListChapters(bookID)
	if err != nil {
		return nil
	}
	// Concatenate the lowercased text of the chapters the reader has read.
	var read strings.Builder
	for _, ch := range chapters {
		if ch.Index > chapter {
			continue
		}
		if full, _ := s.store.GetChapterContent(bookID, ch.Index); full != nil {
			read.WriteString(strings.ToLower(full.Content))
			read.WriteByte('\n')
		}
	}
	hay := read.String()
	if hay == "" {
		return nil
	}

	var out []qaSuggestion
	for i, c := range chars {
		if len(out) >= 2 || i >= 8 { // only the top few, bounded work
			break
		}
		name := strings.TrimSpace(c.Name)
		if name == "" || !nameAppearsIn(hay, name, c.Aliases) {
			continue
		}
		short := shortCharName(name)
		out = append(out, qaSuggestion{
			Label:  fmt.Sprintf("Who is %s?", short),
			Prompt: fmt.Sprintf("Who is %s again? Remind me who they are, without spoilers.", short),
		})
	}
	return out
}

// nameAppearsIn reports whether the character's name (or a distinctive alias)
// occurs in the already-read text. Matches the full name and its individual
// words ≥ 4 chars (so "Charlie Marlow" is found via "marlow") plus aliases.
func nameAppearsIn(hayLower, name string, aliases []string) bool {
	cands := []string{name}
	cands = append(cands, aliases...)
	cands = append(cands, strings.Fields(name)...)
	for _, cand := range cands {
		c := strings.ToLower(strings.TrimSpace(cand))
		if len(c) >= 4 && strings.Contains(hayLower, c) {
			return true
		}
	}
	return false
}

// shortCharName keeps a long canonical name (BookNLP sometimes yields the full
// "Józef Teodor Konrad Korzeniowski") readable on a chip: first + last word.
func shortCharName(name string) string {
	f := strings.Fields(name)
	if len(f) <= 2 {
		return name
	}
	return f[0] + " " + f[len(f)-1]
}
