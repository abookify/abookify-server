package library

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// bookCountAt reports how many book rows exist with the given path.
func bookCountAt(t *testing.T, store *db.Store, path string) int {
	t.Helper()
	books, err := store.ListBooks()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range books {
		if b.Path == path {
			n++
		}
	}
	return n
}

// TestWatcherRemove_ReachableRootDeletesBook: a file deleted from a still-mounted
// root is a genuine user delete → the book row is removed.
func TestWatcherRemove_ReachableRootDeletesBook(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	MarkRootReachable(root) // sentinel present → root is reachable

	p := filepath.Join(root, "book.mp3")
	os.WriteFile(p, []byte("x"), 0o644)
	if err := store.UpsertBook(db.Book{Path: p, Filename: "book.mp3", Format: "mp3", MediaType: "audio"}); err != nil {
		t.Fatal(err)
	}
	os.Remove(p) // user deletes the file

	w := &Watcher{store: store, root: root}
	if !w.handleRemoved(p) {
		t.Fatal("handleRemoved returned false — a file gone from a reachable root should delete its book")
	}
	if bookCountAt(t, store, p) != 0 {
		t.Error("book row should be gone after the file was deleted from a reachable root")
	}
}

// TestWatcherRemove_UnreachableRootKeepsBooks: THE CRITICAL #220 CASE — the whole
// root is unreachable (unplugged drive, no sentinel). Even though the file no
// longer stats, the book row MUST survive (the reconcile marks it stale, never
// deletes). This is the failure that would actually hurt PJ, so it's asserted
// directly.
func TestWatcherRemove_UnreachableRootKeepsBooks(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	// NO MarkRootReachable → no sentinel → RootReachable(root) is false.

	p := filepath.Join(root, "book.mp3")
	// The file never exists on disk here (drive is "unplugged").
	if err := store.UpsertBook(db.Book{Path: p, Filename: "book.mp3", Format: "mp3", MediaType: "audio"}); err != nil {
		t.Fatal(err)
	}

	w := &Watcher{store: store, root: root}
	if w.handleRemoved(p) {
		t.Fatal("handleRemoved deleted a book under an UNREACHABLE root — an unplugged drive must never lose metadata")
	}
	if bookCountAt(t, store, p) != 1 {
		t.Error("book row must survive when its whole root is unreachable")
	}
}

// TestWatcherRemove_LastBookRemovesWork: deleting a work's last book file (root
// reachable) removes the now-empty work too.
func TestWatcherRemove_LastBookRemovesWork(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	MarkRootReachable(root)

	p := filepath.Join(root, "only.mp3")
	os.WriteFile(p, []byte("x"), 0o644)
	store.UpsertBook(db.Book{Path: p, Filename: "only.mp3", Format: "mp3", MediaType: "audio"})
	books, _ := store.ListBooks()
	var bookID int64
	for _, b := range books {
		if b.Path == p {
			bookID = b.ID
		}
	}
	workID, err := store.CreateWork("Only", "Author")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AssignBooksToWork(workID, []int64{bookID}); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)

	w := &Watcher{store: store, root: root}
	if !w.handleRemoved(p) {
		t.Fatal("handleRemoved should delete the book")
	}
	if wk, _ := store.GetWork(workID); wk != nil {
		t.Error("work should be removed once its last book file is deleted")
	}
}
