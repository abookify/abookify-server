package library

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pj/abookify/internal/db"
)

func TestEnsureRootsMigratesSinglePath(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()

	// A pre-#220 book already in the DB under the (soon-to-be) root.
	if err := store.UpsertBook(db.Book{
		Path: filepath.Join(root, "book.mp3"), Filename: "book.mp3",
		Format: "mp3", MediaType: "audio",
	}); err != nil {
		t.Fatal(err)
	}

	if err := EnsureRoots(store, root); err != nil {
		t.Fatal(err)
	}
	roots, _ := store.ListRoots()
	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1", len(roots))
	}
	if roots[0].Path != root || !roots[0].IsDefault {
		t.Errorf("root = %+v; want path=%s is_default=true", roots[0], root)
	}
	total, stale, _ := store.CountBooksUnderRoot(roots[0].ID)
	if total != 1 || stale != 0 {
		t.Errorf("counts total=%d stale=%d, want 1/0 (book assigned to root)", total, stale)
	}
	// The migrated local root has content → sentinel written → reachable.
	if !RootReachable(root) {
		t.Error("migrated root should be reachable (sentinel present)")
	}
	// Idempotent: a second call adds nothing.
	if err := EnsureRoots(store, root); err != nil {
		t.Fatal(err)
	}
	if roots2, _ := store.ListRoots(); len(roots2) != 1 {
		t.Errorf("EnsureRoots not idempotent: %d roots", len(roots2))
	}
}

func TestRootReachableSentinel(t *testing.T) {
	dir := t.TempDir()
	if RootReachable(dir) {
		t.Error("a dir without the sentinel must read as unreachable (unmounted-stub safety)")
	}
	MarkRootReachable(dir)
	if !RootReachable(dir) {
		t.Error("after MarkRootReachable the dir should be reachable")
	}
	if RootReachable(filepath.Join(dir, "does-not-exist")) {
		t.Error("a missing path must be unreachable")
	}
	// Simulate an unmount: sentinel gone → unreachable (books would be marked
	// stale, NEVER deleted).
	os.Remove(filepath.Join(dir, rootSentinel))
	if RootReachable(dir) {
		t.Error("without the sentinel (drive unplugged) the root must be unreachable")
	}
}

func TestSetRootStaleAndReassign(t *testing.T) {
	store := testStoreForLib(t)
	root := t.TempDir()
	store.UpsertBook(db.Book{Path: filepath.Join(root, "a.mp3"), Filename: "a.mp3", Format: "mp3", MediaType: "audio"})
	id, _ := store.AddRoot(root, "R")
	store.AssignBooksToRoot(id, root)

	if n, _ := store.SetRootStale(id, true); n != 1 {
		t.Errorf("SetRootStale(true) changed %d, want 1", n)
	}
	if _, stale, _ := store.CountBooksUnderRoot(id); stale != 1 {
		t.Error("book should be stale after SetRootStale(true)")
	}
	if n, _ := store.SetRootStale(id, false); n != 1 {
		t.Errorf("SetRootStale(false) changed %d, want 1", n)
	}
	if _, stale, _ := store.CountBooksUnderRoot(id); stale != 0 {
		t.Error("book should be un-stale after SetRootStale(false)")
	}
}
