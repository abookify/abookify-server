package db

import (
	"testing"
)

func TestChatSessionsLifecycle(t *testing.T) {
	store := testStore(t)

	workID, err := store.CreateWork("Test Book", "")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	// Empty list to start.
	sessions, err := store.ListSessions(workID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions, got %d", len(sessions))
	}

	// Create two sessions.
	s1, err := store.CreateSession(workID, "", "reading")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	s2, err := store.CreateSession(workID, "Custom title", "book")
	if err != nil {
		t.Fatalf("create session 2: %v", err)
	}

	sessions, _ = store.ListSessions(workID)
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}

	// Append messages, then list.
	if _, err := store.AppendMessage(s1, "user", "what is this about?", "", ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if _, err := store.AppendMessage(s1, "assistant", "It's a story.", `[{"chapter_idx":0}]`, ""); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	msgs, err := store.ListMessages(s1)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("messages out of order: %v", msgs)
	}
	if msgs[1].CitationsJSON == "" {
		t.Fatalf("assistant citations not persisted")
	}

	// Rename + verify.
	if err := store.RenameSession(s2, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := store.GetSession(s2)
	if err != nil || got == nil || got.Title != "Renamed" {
		t.Fatalf("get session after rename: %+v err=%v", got, err)
	}

	// Append to s1 — its updated_at must bump above s2's so it sorts first.
	if _, err := store.AppendMessage(s1, "user", "follow up", "", ""); err != nil {
		t.Fatalf("append followup: %v", err)
	}
	sessions, _ = store.ListSessions(workID)
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions still, got %d", len(sessions))
	}
	if sessions[0].ID != s1 {
		t.Fatalf("most-recent session should be s1=%d, got %d", s1, sessions[0].ID)
	}

	// Delete s2; messages of s1 stay.
	if err := store.DeleteSession(s2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	sessions, _ = store.ListSessions(workID)
	if len(sessions) != 1 || sessions[0].ID != s1 {
		t.Fatalf("after delete: want only s1=%d, got %v", s1, sessions)
	}
	msgs, _ = store.ListMessages(s1)
	if len(msgs) != 3 {
		t.Fatalf("s1 messages after deleting s2: want 3, got %d", len(msgs))
	}

	// Delete the work — both session and messages should be removed too.
	if err := store.DeleteWork(workID); err != nil {
		t.Fatalf("delete work: %v", err)
	}
	sessions, _ = store.ListSessions(workID)
	if len(sessions) != 0 {
		t.Fatalf("sessions should cascade-delete with work, got %d", len(sessions))
	}
	msgs, _ = store.ListMessages(s1)
	if len(msgs) != 0 {
		t.Fatalf("messages should cascade-delete with work, got %d", len(msgs))
	}
}
