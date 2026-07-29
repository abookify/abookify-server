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

// helper: make a root dir with the reachability sentinel + N book files that exist.
func seedRoot(t *testing.T, store *db.Store, reachable bool, presentFiles, missingCount int) (string, int64) {
	t.Helper()
	root := t.TempDir()
	if reachable {
		MarkRootReachable(root)
	}
	id, _ := store.AddRoot(root, "R")
	for i := 0; i < presentFiles; i++ {
		p := filepath.Join(root, "present-"+string(rune('a'+i))+".mp3")
		os.WriteFile(p, []byte("x"), 0o644)
		store.UpsertBook(db.Book{Path: p, Filename: filepath.Base(p), Format: "mp3", MediaType: "audio"})
	}
	for i := 0; i < missingCount; i++ {
		p := filepath.Join(root, "gone-"+string(rune('a'+i))+".mp3") // never created on disk
		store.UpsertBook(db.Book{Path: p, Filename: filepath.Base(p), Format: "mp3", MediaType: "audio"})
	}
	store.AssignBooksToRoot(id, root)
	return root, id
}

func TestReconcile_UnreachableMarksStaleNeverDeletes(t *testing.T) {
	store := testStoreForLib(t)
	// reachable=false → NO sentinel. Books' files even exist on disk, but the
	// root reads as unplugged, so nothing must be deleted.
	_, id := seedRoot(t, store, false, 2, 0)
	staleRoots, removed := ReconcileLibraryRoots(store)
	if removed != 0 {
		t.Fatalf("removed=%d — an unreachable root must NEVER delete books", removed)
	}
	if staleRoots != 1 {
		t.Errorf("staleRoots=%d, want 1", staleRoots)
	}
	total, stale, _ := store.CountBooksUnderRoot(id)
	if total != 2 || stale != 2 {
		t.Errorf("total=%d stale=%d, want 2/2 (kept + marked stale)", total, stale)
	}
}

func TestReconcile_ReachableDeletesOnlyMissing(t *testing.T) {
	store := testStoreForLib(t)
	_, id := seedRoot(t, store, true, 2, 1) // 2 present, 1 genuinely missing
	_, removed := ReconcileLibraryRoots(store)
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 (only the genuinely-missing file)", removed)
	}
	total, stale, _ := store.CountBooksUnderRoot(id)
	if total != 2 || stale != 0 {
		t.Errorf("total=%d stale=%d, want 2/0 (present kept, not stale)", total, stale)
	}
}

func TestReconcile_MassMissingMarksStaleNotDeleted(t *testing.T) {
	store := testStoreForLib(t)
	// reachable sentinel but almost everything missing → mount glitch → stale.
	_, id := seedRoot(t, store, true, 1, 20)
	_, removed := ReconcileLibraryRoots(store)
	if removed != 0 {
		t.Fatalf("removed=%d — mass-missing under a reachable root must mark stale, not delete", removed)
	}
	total, _, _ := store.CountBooksUnderRoot(id)
	if total != 21 {
		t.Errorf("total=%d, want 21 (nothing deleted)", total)
	}
}
