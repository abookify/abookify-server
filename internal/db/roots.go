package db

// Library roots (#220) — the N filesystem locations the scanner walks. CRUD +
// the book↔root helpers the reconciler uses to mark an unreachable root's books
// stale (rather than deleting them). Reachability itself is computed in the
// library package (it needs the filesystem + a sentinel file).

import "time"

// LibraryRoot is one configured library location.
type LibraryRoot struct {
	ID        int64     `json:"id"`
	Path      string    `json:"path"`
	Label     string    `json:"label"`
	IsDefault bool      `json:"is_default"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
}

// ListRoots returns the configured library roots in display order.
func (s *Store) ListRoots() ([]LibraryRoot, error) {
	rows, err := s.db.Query(`SELECT id, path, label, is_default, position, created_at
		FROM library_roots ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibraryRoot
	for rows.Next() {
		var r LibraryRoot
		var isDef int
		if err := rows.Scan(&r.ID, &r.Path, &r.Label, &isDef, &r.Position, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsDefault = isDef != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddRoot inserts a new root at the end. The first root added becomes the
// default. Returns the new id. Path must be unique (enforced by the schema).
func (s *Store) AddRoot(path, label string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n, maxPos int
	tx.QueryRow(`SELECT COUNT(*), COALESCE(MAX(position), -1) FROM library_roots`).Scan(&n, &maxPos)
	isDefault := 0
	if n == 0 {
		isDefault = 1 // first root is the default import target
	}
	res, err := tx.Exec(`INSERT INTO library_roots (path, label, is_default, position) VALUES (?, ?, ?, ?)`,
		path, label, isDefault, maxPos+1)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// RemoveRoot deletes a root. Its books are NOT deleted — they're unassigned
// (root_id = 0) so no metadata is lost; the user can re-add the root or purge
// them explicitly.
func (s *Store) RemoveRoot(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE books SET root_id = 0 WHERE root_id = ?`, id); err != nil {
		return err
	}
	var wasDefault int
	tx.QueryRow(`SELECT is_default FROM library_roots WHERE id = ?`, id).Scan(&wasDefault)
	if _, err := tx.Exec(`DELETE FROM library_roots WHERE id = ?`, id); err != nil {
		return err
	}
	// If we removed the default, promote the first remaining root.
	if wasDefault != 0 {
		tx.Exec(`UPDATE library_roots SET is_default = 1
			WHERE id = (SELECT id FROM library_roots ORDER BY position, id LIMIT 1)`)
	}
	return tx.Commit()
}

// SetDefaultRoot makes one root the default import target (exactly one default).
func (s *Store) SetDefaultRoot(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE library_roots SET is_default = 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE library_roots SET is_default = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ReorderRoots sets positions from the given id order.
func (s *Store) ReorderRoots(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for pos, id := range ids {
		if _, err := tx.Exec(`UPDATE library_roots SET position = ? WHERE id = ?`, pos, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DefaultRoot returns the default import-target root, or nil if none configured.
func (s *Store) DefaultRoot() (*LibraryRoot, error) {
	roots, err := s.ListRoots()
	if err != nil {
		return nil, err
	}
	for i := range roots {
		if roots[i].IsDefault {
			return &roots[i], nil
		}
	}
	if len(roots) > 0 {
		return &roots[0], nil
	}
	return nil, nil
}

// CountBooksUnderRoot returns visible book counts for a root: total + stale.
func (s *Store) CountBooksUnderRoot(rootID int64) (total, stale int, err error) {
	err = s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN stale != 0 THEN 1 ELSE 0 END), 0)
		FROM books WHERE root_id = ? AND visibility != 'internal'`, rootID).Scan(&total, &stale)
	return
}

// AssignBooksToRoot sets root_id for every book whose path starts with prefix+"/"
// (or equals prefix) and is currently unassigned. Used to migrate the pre-#220
// single-root library. Returns rows affected.
func (s *Store) AssignBooksToRoot(rootID int64, prefix string) (int64, error) {
	res, err := s.db.Exec(`UPDATE books SET root_id = ?
		WHERE root_id = 0 AND (path = ? OR path LIKE ? || '/%')`, rootID, prefix, prefix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetRootStale marks (or clears) the stale flag on every book under a root —
// used when a root goes unreachable / comes back. Returns rows changed.
func (s *Store) SetRootStale(rootID int64, stale bool) (int64, error) {
	v := 0
	if stale {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE books SET stale = ? WHERE root_id = ? AND stale != ?`, v, rootID, v)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// BooksUnderRoot returns (id, path) for every book under a root — the reconciler
// walks these to delete genuinely-missing files when the root IS reachable.
func (s *Store) BooksUnderRoot(rootID int64) ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, path, stale FROM books WHERE root_id = ?`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		var b Book
		var stale int
		if err := rows.Scan(&b.ID, &b.Path, &stale); err != nil {
			return nil, err
		}
		b.Stale = stale != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBookStale flips a single book's stale flag.
func (s *Store) SetBookStale(bookID int64, stale bool) error {
	v := 0
	if stale {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE books SET stale = ? WHERE id = ?`, v, bookID)
	return err
}
