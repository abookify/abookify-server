package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SourceProvenance is a human's DECLARATION about one source — and, separately,
// whether it is CLEARED for public redistribution. The two are distinct on
// purpose (2026-09-18): the showcase's Owl Creek recording HAD a declaration
// whose own words were "verify before public redistribution", and it was
// published anyway. A publish gate that checks only for a declaration would
// have passed it. Cleared is the flag a person sets after actually verifying;
// ClearedBy names them; Note holds the reasoning. Nothing here is inferred.
//
// Scope is "book" (RefID = books.id) or "cover" (RefID = works.id — the work's
// served cover file). Kind is free-form but the publish gate recognises:
// gutenberg, librivox, pg-open-audiobook, kokoro (our own TTS from a cleared
// text), cc0, cc-by, own — and treats anything else as needing an explicit
// clearance with a note.
type SourceProvenance struct {
	Scope     string `json:"scope"`
	RefID     int64  `json:"ref_id"`
	Kind      string `json:"kind"`
	SourceURL string `json:"source_url,omitempty"`
	License   string `json:"license,omitempty"`
	Cleared   bool   `json:"cleared"`
	ClearedBy string `json:"cleared_by,omitempty"`
	ClearedAt string `json:"cleared_at,omitempty"`
	Note      string `json:"note,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// SetSourceProvenance upserts one declaration. Setting Cleared=true without a
// ClearedBy is refused: a clearance nobody signs is not a clearance.
func (s *Store) SetSourceProvenance(p SourceProvenance) error {
	p.Scope = strings.ToLower(strings.TrimSpace(p.Scope))
	if p.Scope != "book" && p.Scope != "cover" {
		return fmt.Errorf("provenance scope must be book or cover, got %q", p.Scope)
	}
	if p.Cleared && strings.TrimSpace(p.ClearedBy) == "" {
		return fmt.Errorf("a clearance must name who cleared it (cleared_by)")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	clearedAt := ""
	if p.Cleared {
		clearedAt = now
		if p.ClearedAt != "" {
			clearedAt = p.ClearedAt
		}
	}
	_, err := s.db.Exec(`
		INSERT INTO source_provenance (scope, ref_id, kind, source_url, license, cleared, cleared_by, cleared_at, note, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, ref_id) DO UPDATE SET
			kind=excluded.kind, source_url=excluded.source_url, license=excluded.license,
			cleared=excluded.cleared, cleared_by=excluded.cleared_by, cleared_at=excluded.cleared_at,
			note=excluded.note, updated_at=excluded.updated_at`,
		p.Scope, p.RefID, p.Kind, p.SourceURL, p.License, boolInt(p.Cleared), p.ClearedBy, clearedAt, p.Note, now)
	return err
}

// GetSourceProvenance returns the declarations for the given ids in one scope;
// ids with no row are absent from the map (absence = undeclared = not cleared).
func (s *Store) GetSourceProvenance(scope string, ids []int64) (map[int64]SourceProvenance, error) {
	out := map[int64]SourceProvenance{}
	for _, id := range ids {
		var p SourceProvenance
		var cleared int
		err := s.db.QueryRow(`SELECT scope, ref_id, kind, source_url, license, cleared, cleared_by, cleared_at, note, updated_at
			FROM source_provenance WHERE scope=? AND ref_id=?`, scope, id).
			Scan(&p.Scope, &p.RefID, &p.Kind, &p.SourceURL, &p.License, &cleared, &p.ClearedBy, &p.ClearedAt, &p.Note, &p.UpdatedAt)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		p.Cleared = cleared != 0
		out[id] = p
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
