package db

import (
	"testing"
	"time"

	"github.com/pj/abookify/internal/applog"
)

func TestLogsInsertQueryFilters(t *testing.T) {
	store := testStore(t)
	now := time.Now()

	entries := []applog.Entry{
		{Time: now.Add(-2 * time.Hour), Level: applog.LevelInfo, Component: "system", Message: "old startup line"},
		{Time: now.Add(-30 * time.Minute), Level: applog.LevelDebug, Component: "http", Message: "GET /api/health"},
		{Time: now.Add(-10 * time.Minute), Level: applog.LevelInfo, Component: "jobs",
			JobID: "stt-redo-12", WorkID: 42, Message: "job started",
			Fields: map[string]any{"type": "stt"}},
		{Time: now.Add(-1 * time.Minute), Level: applog.LevelError, Component: "jobs",
			JobID: "stt-redo-12", WorkID: 42, Message: "job failed: connection refused",
			Fields: map[string]any{"type": "stt", "error": "connection refused"}},
	}
	if err := store.InsertLogs(entries); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	// Newest-first ordering across everything.
	all, err := store.QueryLogs(LogFilter{Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 entries, got %d", len(all))
	}
	if all[0].Message != "job failed: connection refused" {
		t.Errorf("newest-first broken: got %q", all[0].Message)
	}

	// Populated columns round-trip: job_id, work_id, fields.
	top := all[0]
	if top.JobID != "stt-redo-12" || top.WorkID != 42 {
		t.Errorf("job/work id lost: job=%q work=%d", top.JobID, top.WorkID)
	}
	if top.Fields["error"] != "connection refused" || top.Fields["type"] != "stt" {
		t.Errorf("fields lost: %#v", top.Fields)
	}
	if top.Time.IsZero() {
		t.Error("timestamp not decoded")
	}

	// Min-level filter: warn+ should drop the info/debug entries.
	warns, err := store.QueryLogs(LogFilter{MinLevel: applog.LevelWarn, Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs warn+: %v", err)
	}
	if len(warns) != 1 || warns[0].Level != applog.LevelError {
		t.Errorf("warn+ filter: want 1 error, got %d (%v)", len(warns), warns)
	}

	// Component filter.
	jobs, err := store.QueryLogs(LogFilter{Component: "jobs", Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs component: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("component=jobs: want 2, got %d", len(jobs))
	}

	// Job id filter.
	byJob, err := store.QueryLogs(LogFilter{JobID: "stt-redo-12", Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs job_id: %v", err)
	}
	if len(byJob) != 2 {
		t.Errorf("job_id filter: want 2, got %d", len(byJob))
	}

	// Work id filter (matches both job-tagged rows; the 2 system rows have work_id=0).
	byWork, err := store.QueryLogs(LogFilter{WorkID: 42, Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs work_id: %v", err)
	}
	if len(byWork) != 2 {
		t.Errorf("work_id filter: want 2, got %d", len(byWork))
	}

	// Message substring.
	q, err := store.QueryLogs(LogFilter{Query: "connection", Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs q: %v", err)
	}
	if len(q) != 1 {
		t.Errorf("q=connection: want 1, got %d", len(q))
	}

	// Since window: last hour drops the 2h-old line.
	recent, err := store.QueryLogs(LogFilter{Since: now.Add(-time.Hour), Limit: 100})
	if err != nil {
		t.Fatalf("QueryLogs since: %v", err)
	}
	if len(recent) != 3 {
		t.Errorf("since=1h: want 3, got %d", len(recent))
	}

	// Distinct components for the UI dropdown.
	comps, err := store.DistinctLogComponents()
	if err != nil {
		t.Fatalf("DistinctLogComponents: %v", err)
	}
	if len(comps) != 3 { // http, jobs, system
		t.Errorf("distinct components: want 3, got %d (%v)", len(comps), comps)
	}
}

func TestLogsPrune(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	if err := store.InsertLogs([]applog.Entry{
		{Time: now.Add(-48 * time.Hour), Level: applog.LevelInfo, Component: "system", Message: "ancient"},
		{Time: now.Add(-1 * time.Minute), Level: applog.LevelInfo, Component: "system", Message: "fresh"},
	}); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	n, err := store.PruneLogs(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if n != 1 {
		t.Errorf("prune count: want 1, got %d", n)
	}

	left, _ := store.QueryLogs(LogFilter{Limit: 100})
	if len(left) != 1 || left[0].Message != "fresh" {
		t.Errorf("after prune want [fresh], got %v", left)
	}
}
