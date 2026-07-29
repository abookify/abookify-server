package library

// Library-root reachability + migration (#220). Reachability uses a sentinel
// file, NOT a bare os.Stat: an unmounted mountpoint often still stats as an
// empty stub directory, which would read as "reachable but empty" and make the
// orphan cleanup delete every book under it. The sentinel lives ON the drive, so
// it's absent when the drive is unplugged — the safe signal.

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// rootSentinel is written into a root once we've confirmed it's genuinely
// present with content; its absence means the root is unreachable.
const rootSentinel = ".abookify-root"

// RootReachable reports whether a library root is currently mounted + ours: the
// path is an existing directory AND our sentinel file is present. A missing
// sentinel (unmounted drive, or an empty stub mountpoint) reads as unreachable —
// which the reconciler treats as "mark books stale", never "delete".
func RootReachable(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, rootSentinel)); err != nil {
		return false
	}
	return true
}

// MarkRootReachable writes the sentinel into a root — call this only when the
// root is confirmed mounted with content (a scan found files, or the migrated
// single library already has books), so an unmounted stub never gets one.
// Best-effort: a write failure just leaves the root reading as unreachable.
func MarkRootReachable(path string) {
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return
	}
	f := filepath.Join(path, rootSentinel)
	if _, err := os.Stat(f); err == nil {
		return // already present
	}
	_ = os.WriteFile(f, []byte("abookify library root — do not delete; marks the drive as present\n"), 0o644)
}

// EnsureRoots migrates the pre-#220 single library path in as root #1 the first
// time it runs (no roots configured yet), assigning existing books to it, and
// marks it reachable when it has content. Idempotent — a no-op once roots exist.
func EnsureRoots(store *db.Store, defaultPath string) error {
	roots, err := store.ListRoots()
	if err != nil {
		return err
	}
	if len(roots) > 0 || defaultPath == "" {
		return nil
	}
	id, err := store.AddRoot(defaultPath, "Library")
	if err != nil {
		return err
	}
	n, _ := store.AssignBooksToRoot(id, defaultPath)
	log.Printf("#220: migrated single library path as root #1 (%s); assigned %d existing book(s)", defaultPath, n)
	// The seed path is the local -library dir (never an unmounted stub), so it's
	// safe to mark it reachable when the directory exists.
	if _, err := os.Stat(defaultPath); err == nil {
		MarkRootReachable(defaultPath)
	}
	return nil
}

// ReconcileLibraryRoots is the boot reconcile that REPLACES the old
// "delete any book whose file is missing" cleanup (the mass-deletion hazard).
// For each root:
//   - unreachable (unplugged / no sentinel): mark ALL its books stale, delete
//     NOTHING — losing an unplugged drive's metadata is the worst-case failure.
//   - reachable: clear stale, then delete only books whose file is genuinely
//     gone — UNLESS a suspiciously large fraction is missing (a partial/odd mount
//     despite the sentinel), in which case mark stale instead of deleting.
//
// Books with no root (root_id = 0, including generated:// virtuals) are left
// untouched. Returns (staleRoots, removedBooks).
func ReconcileLibraryRoots(store *db.Store) (staleRoots, removed int) {
	roots, err := store.ListRoots()
	if err != nil {
		log.Printf("#220: reconcile: list roots failed: %v", err)
		return
	}
	for i := range roots {
		r := roots[i]
		if !RootReachable(r.Path) {
			if n, _ := store.SetRootStale(r.ID, true); n > 0 {
				log.Printf("#220: root %q unreachable — marked %d book(s) STALE (not deleted)", r.Path, n)
			}
			staleRoots++
			continue
		}
		books, err := store.BooksUnderRoot(r.ID)
		if err != nil {
			continue
		}
		var missing []int64
		for _, b := range books {
			if strings.HasPrefix(b.Path, "generated://") {
				continue
			}
			if _, err := os.Stat(b.Path); os.IsNotExist(err) {
				missing = append(missing, b.ID)
			}
		}
		// A "reachable" root that's suddenly missing a big chunk of its files is
		// far more likely a mount glitch than the user deleting everything —
		// mark stale, never mass-delete.
		if len(missing) > 10 && len(missing)*100 >= len(books)*40 {
			store.SetRootStale(r.ID, true)
			staleRoots++
			log.Printf("#220: root %q reachable but %d/%d files missing — marking STALE, NOT deleting (looks like a partial mount)", r.Path, len(missing), len(books))
			continue
		}
		store.SetRootStale(r.ID, false)
		for _, id := range missing {
			if err := store.DeleteBook(id); err == nil {
				removed++
			}
		}
		if removed > 0 {
			log.Printf("#220: root %q — removed %d genuinely-missing book(s)", r.Path, len(missing))
		}
	}
	// Deleting a root's last missing book can leave its work bookless — sweep
	// those phantom shells so a vanished file doesn't leave a broken work card.
	if n, err := store.DeleteEmptyWorks(); err != nil {
		log.Printf("#220: reconcile empty-work sweep failed: %v", err)
	} else if n > 0 {
		log.Printf("#220: reconcile removed %d empty work(s)", n)
	}
	return
}

// RootForPath returns the root whose path is the longest prefix of p, or nil.
// Longest-match so a (rejected) nested root still resolves deterministically.
func RootForPath(p string, roots []db.LibraryRoot) *db.LibraryRoot {
	var best *db.LibraryRoot
	for i := range roots {
		rp := roots[i].Path
		if p == rp || (len(p) > len(rp) && p[:len(rp)] == rp && p[len(rp)] == filepath.Separator) {
			if best == nil || len(rp) > len(best.Path) {
				best = &roots[i]
			}
		}
	}
	return best
}
