package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/db"
)

// handleListLogs serves the System Console (#214). Auth-gated by virtue
// of living under /api/ (see isAuthExempt). Returns the recent window
// newest-first plus the set of components present, for the filter UI.
//
// Query params (all optional):
//
//	level=info        minimum severity (debug|info|warn|error)
//	component=jobs    exact component
//	job_id=stt-redo-12 exact job id
//	q=connection      case-insensitive message substring
//	since=1h          relative window (Go duration) or RFC3339 timestamp
//	limit=200         max rows (capped at 2000)
func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := db.LogFilter{
		Component: strings.TrimSpace(q.Get("component")),
		JobID:     strings.TrimSpace(q.Get("job_id")),
		Query:     strings.TrimSpace(q.Get("q")),
	}

	switch strings.ToLower(strings.TrimSpace(q.Get("level"))) {
	case "debug":
		f.MinLevel = applog.LevelDebug
	case "info":
		f.MinLevel = applog.LevelInfo
	case "warn", "warning":
		f.MinLevel = applog.LevelWarn
	case "error", "err":
		f.MinLevel = applog.LevelError
	}

	if since := strings.TrimSpace(q.Get("since")); since != "" {
		if d, err := time.ParseDuration(since); err == nil {
			f.Since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, since); err == nil {
			f.Since = t
		}
	}

	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil {
		f.Limit = n
	}

	logs, err := s.store.QueryLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if logs == nil {
		logs = []applog.Entry{}
	}
	components, _ := s.store.DistinctLogComponents()
	if components == nil {
		components = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"logs":       logs,
		"components": components,
	})
}
