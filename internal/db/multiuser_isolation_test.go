package db

import (
	"path/filepath"
	"testing"
)

// Multi-user isolation is security-critical and silent when it breaks: a write
// leak leaves no crash, no error, no log — just one reader's data under another's
// name. This locks the three properties down so a refactor can't reopen the hole:
// positions are per-reader; a reader can't READ another's bookmarks/Q&A; and — the
// one most likely to be missed because it's invisible — a reader can't MUTATE
// another's.
func TestMultiUserIsolationAndOwnership(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "iso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	alice, err := store.CreateUser("alice", "h1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser("bob", "h2")
	if err != nil {
		t.Fatal(err)
	}
	wid, err := store.CreateWork("Shared Book", "Author")
	if err != nil {
		t.Fatal(err)
	}

	// --- positions: separate per reader on the SAME book ---
	if err := store.SavePosition(PlaybackPosition{WorkID: wid, BookID: 1, PositionSecs: 100}, alice); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePosition(PlaybackPosition{WorkID: wid, BookID: 1, PositionSecs: 250}, bob); err != nil {
		t.Fatal(err)
	}
	pa, _ := store.GetPosition(wid, alice)
	pb, _ := store.GetPosition(wid, bob)
	if pa == nil || pa.PositionSecs != 100 || pb == nil || pb.PositionSecs != 250 {
		t.Fatalf("positions not isolated: alice=%v bob=%v (want 100 / 250)", pa, pb)
	}

	// --- bookmarks: read isolation + mutate ownership ---
	bmID, err := store.CreateBookmark(Bookmark{WorkID: wid, BookID: 1, Type: "bookmark", Note: "alice's note"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	if bms, _ := store.ListBookmarks(wid, bob); len(bms) != 0 {
		t.Errorf("READ LEAK: bob sees alice's bookmarks (%d)", len(bms))
	}
	// bob tries to mutate alice's bookmark — must be a no-op (the write-leak case)
	store.UpdateBookmark(bmID, "bob was here", "", bob)
	store.DeleteBookmark(bmID, bob)
	after, _ := store.ListBookmarks(wid, alice)
	if len(after) != 1 {
		t.Fatalf("WRITE LEAK: bob deleted alice's bookmark (alice now has %d)", len(after))
	}
	if after[0].Note != "alice's note" {
		t.Errorf("WRITE LEAK: bob edited alice's bookmark note → %q", after[0].Note)
	}

	// --- Q&A sessions: read isolation + mutate ownership ---
	sid, err := store.CreateSession(wid, "alice's private chat", "reading", alice)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSession(sid, bob); got != nil {
		t.Errorf("READ LEAK: bob read alice's session by id")
	}
	if ss, _ := store.ListSessions(wid, bob); len(ss) != 0 {
		t.Errorf("READ LEAK: bob lists alice's sessions (%d)", len(ss))
	}
	// bob tries to mutate/delete alice's session — must be no-ops
	store.RenameSession(sid, "bob renamed this", bob)
	store.SetSessionScope(sid, "book", bob)
	store.DeleteSession(sid, bob)
	own, _ := store.GetSession(sid, alice)
	if own == nil {
		t.Fatalf("WRITE LEAK: bob deleted alice's session")
	}
	if own.Title != "alice's private chat" {
		t.Errorf("WRITE LEAK: bob renamed alice's session → %q", own.Title)
	}
	if own.Scope != "reading" {
		t.Errorf("WRITE LEAK: bob changed alice's session scope → %q", own.Scope)
	}
}
