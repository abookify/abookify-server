package db

// Audio-timing soundness results. The probe (testing/timing-probe.py, GPU whisper
// re-transcribe — transcription's lane) WRITES these per work+edition; the server
// READS them to compute the reader-facing timing tier. Absence = not yet checked.

type TimingResult struct {
	WorkID      int64   `json:"work_id"`
	EditionKey  string  `json:"edition_key"` // audio directory, matching the probe's grouping
	Windows     int     `json:"windows"`     // probe windows that ran
	Passed      int     `json:"passed"`      // windows meeting >=60% word-match + median|Δt|<=1.5s
	MedianDelta float64 `json:"median_delta"`
	CheckedAt   string  `json:"checked_at"`
}

// UpsertTimingResult stores/updates one edition's probe outcome.
func (s *Store) UpsertTimingResult(r TimingResult) error {
	_, err := s.db.Exec(`
		INSERT INTO timing_results (work_id, edition_key, windows, passed, median_delta)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(work_id, edition_key) DO UPDATE SET
			windows=excluded.windows, passed=excluded.passed,
			median_delta=excluded.median_delta, checked_at=CURRENT_TIMESTAMP`,
		r.WorkID, r.EditionKey, r.Windows, r.Passed, r.MedianDelta)
	return err
}

// GetTimingResults returns every edition's probe result for a work, keyed by
// edition_key (the audio directory).
func (s *Store) GetTimingResults(workID int64) (map[string]TimingResult, error) {
	rows, err := s.db.Query(
		`SELECT edition_key, windows, passed, median_delta, checked_at
		 FROM timing_results WHERE work_id = ?`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TimingResult{}
	for rows.Next() {
		r := TimingResult{WorkID: workID}
		if err := rows.Scan(&r.EditionKey, &r.Windows, &r.Passed, &r.MedianDelta, &r.CheckedAt); err != nil {
			return nil, err
		}
		out[r.EditionKey] = r
	}
	return out, rows.Err()
}
