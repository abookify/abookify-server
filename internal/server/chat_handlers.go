// HTTP handlers for chat sessions.
//
// API surface:
//   GET    /api/works/{id}/sessions          — list sessions for a work
//   POST   /api/works/{id}/sessions          — create a new session
//   GET    /api/sessions/{id}/messages       — list messages in a session
//   POST   /api/sessions/{id}/messages       — append a user msg + return assistant
//   PUT    /api/sessions/{id}                — rename a session
//   DELETE /api/sessions/{id}                — delete a session
package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/llm"
)

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	sessions, err := s.store.ListSessions(workID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []db.QASession{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Title string `json:"title"`
		Scope string `json:"scope"` // "reading" (default, spoiler-safe) | "book"
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	id, err := s.store.CreateSession(workID, req.Title, req.Scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sess, _ := s.store.GetSession(id)
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.store.RenameSession(id, req.Title); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sess, _ := s.store.GetSession(id)
	writeJSON(w, http.StatusOK, sess)
}

// handleSetSessionScope changes a chat's spoiler scope (#130): "reading"
// (answer only up to the reader's position) or "book" (whole book).
func (s *Server) handleSetSessionScope(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.store.SetSessionScope(id, req.Scope); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sess, _ := s.store.GetSession(id)
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.store.DeleteSession(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// chatMessage is the wire format for messages — adds parsed citations
// (omitted from db.QAMessage's JSON because we store them as a string)
// and the snapshot scope for user turns.
type chatMessage struct {
	ID        int64               `json:"id"`
	SessionID int64               `json:"session_id"`
	Role      string              `json:"role"`
	Content   string              `json:"content"`
	Citations []llm.Citation      `json:"citations,omitempty"`
	Scope     *library.QueryScope `json:"scope,omitempty"`
	CreatedAt string              `json:"created_at"`
}

func toChatMessage(m db.QAMessage) chatMessage {
	cm := chatMessage{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Content:   m.Content,
		Citations: library.UnmarshalCitations(m.CitationsJSON),
		CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if m.ScopeJSON != "" {
		var sc library.QueryScope
		if err := json.Unmarshal([]byte(m.ScopeJSON), &sc); err == nil && sc.Type != "" && sc.Type != "book" {
			cm.Scope = &sc
		}
	}
	return cm
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	sessionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	msgs, err := s.store.ListMessages(sessionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toChatMessage(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAppendMessage runs one full ask-and-answer round in a session:
// stores the user's message, retrieves passages + history-aware completion,
// stores the assistant reply, and returns the assistant's chatMessage so
// the client can append it to the visible thread.
func (s *Server) handleAppendMessage(w http.ResponseWriter, r *http.Request) {
	rag := s.RAG()
	if rag == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "No LLM configured. Add an API key in Settings.",
		})
		return
	}
	sessionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	sess, err := s.store.GetSession(sessionID)
	if err != nil || sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	var req struct {
		Content string             `json:"content"`
		Scope   library.QueryScope `json:"scope,omitempty"` // optional per-turn narrowing (paragraph/chapter)
		// Reader's live position, for the session's spoiler scope (#130). The
		// server — not the client — resolves the effective bound, so spoiler
		// safety is enforced server-side.
		ReaderBookID  int64 `json:"reader_book_id,omitempty"`
		ReaderChapter int   `json:"reader_chapter,omitempty"`
	}
	req.ReaderChapter = -1 // distinguish "not sent" from chapter 0
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty message"})
		return
	}

	// Resolve the effective retrieval scope from the chat's spoiler mode +
	// the reader's live position (#130). "reading" → up_to the current chapter;
	// "book" → whole book; an explicit paragraph/chapter override narrows it.
	scope := library.ResolveSessionScope(sess.Scope, req.ReaderBookID, req.ReaderChapter, req.Scope)

	// Snapshot the prior history *before* appending the new user message —
	// AskInSession appends the question itself.
	history, err := s.store.ListMessages(sessionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Snapshot the resolved scope on the user message so chat history can show
	// what the answer was bounded to. Empty for whole-book turns (default) so
	// we don't bloat the table with no-op blobs.
	scopeJSON := ""
	if scope.Type != "" && scope.Type != "book" {
		if b, err := json.Marshal(scope); err == nil {
			scopeJSON = string(b)
		}
	}

	// Persist the user message first so it shows up immediately if the
	// LLM call fails or times out.
	userID, err := s.store.AppendMessage(sessionID, "user", req.Content, "", scopeJSON)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Auto-name the session from its first user message if it's still the
	// placeholder "New chat".
	if len(history) == 0 && sess.Title == "New chat" {
		_ = s.store.RenameSession(sessionID, library.DeriveSessionTitle(req.Content))
	}

	answer, err := library.AskInSession(s.store, rag, sess.WorkID, history, req.Content, scope)
	if err != nil {
		// Persist a placeholder so the UI shows the failure inline rather
		// than silently dropping the turn.
		errMsg := "I couldn't answer that — " + err.Error()
		assistID, _ := s.store.AppendMessage(sessionID, "assistant", errMsg, "", "")
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id":   userID,
			"assistant": chatMessage{ID: assistID, SessionID: sessionID, Role: "assistant", Content: errMsg},
		})
		return
	}

	citationsJSON := library.MarshalCitations(answer.Citations)
	assistID, err := s.store.AppendMessage(sessionID, "assistant", answer.Text, citationsJSON, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": userID,
		"assistant": chatMessage{
			ID:        assistID,
			SessionID: sessionID,
			Role:      "assistant",
			Content:   answer.Text,
			Citations: answer.Citations,
		},
	})
}
