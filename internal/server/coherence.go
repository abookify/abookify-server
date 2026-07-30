package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pj/abookify/internal/applog"
	"github.com/pj/abookify/internal/library"
)

// Coherence endpoints + the standing sweep.
//
// A repaired book can land with its surfaces disagreeing — the reader shows the
// corrected text while Q&A cites the invented version from a stale chunk. This
// makes "is the repaired book coherent across reader, search, Q&A, sync and
// text-trust" queryable by DATA, and logs it loud after every rescan so nobody
// has to remember to check at 4am. GET /api/works/{id}/coherence (one work),
// GET /api/coherence (whole library, worst first). See library.CheckWorkCoherence.

func (s *Server) handleWorkCoherence(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	wc, err := library.CheckWorkCoherence(s.store, id)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if wc == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work not found"})
		return
	}
	writeJSON(w, http.StatusOK, wc)
}

func (s *Server) handleLibraryCoherence(w http.ResponseWriter, r *http.Request) {
	all, err := library.CheckLibraryCoherence(s.store)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	incoherent, degraded := sortCoherenceWorstFirst(all)
	writeJSON(w, http.StatusOK, map[string]any{
		"checked_works":    len(all),
		"incoherent_count": incoherent,
		"degraded_count":   degraded,
		"works":            all,
	})
}

// sortCoherenceWorstFirst orders incoherent works first, then degraded, then the
// rest — and returns the two counts. Mutates in place.
func sortCoherenceWorstFirst(all []library.WorkCoherence) (incoherent, degraded int) {
	for i := range all {
		if !all[i].Coherent {
			incoherent++
		} else if all[i].Degraded {
			degraded++
		}
	}
	rank := func(c library.WorkCoherence) int {
		switch {
		case !c.Coherent:
			return 0
		case c.Degraded:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		ra, rb := rank(all[a]), rank(all[b])
		if ra != rb {
			return ra < rb
		}
		return len(all[a].Issues) > len(all[b].Issues)
	})
	return incoherent, degraded
}

// sweepCoherence runs the whole-library check and logs LOUD for any work whose
// surfaces disagree. Called after a rescan (a repair lands via ImportSidecars
// there) and at boot, so an incoherent book is surfaced by data rather than by a
// reader noticing Q&A cite a passage that isn't on the page. Best-effort: a check
// failure must never break the sweep that called it.
func (s *Server) sweepCoherence(reason string) {
	all, err := library.CheckLibraryCoherence(s.store)
	if err != nil {
		applog.Warnf("system", "coherence sweep (%s) failed: %v", reason, err)
		return
	}
	incoherent, degraded := 0, 0
	for i := range all {
		wc := &all[i]
		if !wc.Coherent {
			incoherent++
			// Loud: name the work and the disagreeing surfaces. This is the failure —
			// the reader/Q&A/karaoke showing different text (chunks or sync). A stale
			// trust badge is only DEGRADED, so it never reaches here.
			surfaces := map[string]bool{}
			for _, iss := range wc.Issues {
				if iss.Severity == "incoherent" {
					surfaces[iss.Surface] = true
				}
			}
			applog.Warnf("system", "COHERENCE: work %d %q is INCOHERENT — %s disagree with the reader text (repaired book landed with stale derived data)",
				wc.WorkID, wc.Title, strings.Join(sortedKeys(surfaces), "+"))
		} else if wc.Degraded {
			degraded++
		}
	}
	if incoherent > 0 {
		applog.Warnf("system", "coherence sweep (%s): %d INCOHERENT, %d degraded, of %d works", reason, incoherent, degraded, len(all))
	} else {
		applog.Infof("system", "coherence sweep (%s): all %d works coherent (%d degraded)", reason, len(all), degraded)
	}
}

// SweepCoherenceAsync runs a coherence sweep off-thread, single-flight: a burst
// of book completions (the repair landing several in a row) collapses to at most
// two passes. Called from the library-change watcher so each landed book is
// checked without waiting for the slow ticker.
func (s *Server) SweepCoherenceAsync(reason string) {
	s.coherenceMu.Lock()
	if s.coherenceRunning {
		s.coherenceDirty = true
		s.coherenceMu.Unlock()
		return
	}
	s.coherenceRunning = true
	s.coherenceMu.Unlock()
	go func() {
		for {
			s.sweepCoherence(reason)
			s.coherenceMu.Lock()
			if !s.coherenceDirty {
				s.coherenceRunning = false
				s.coherenceMu.Unlock()
				return
			}
			s.coherenceDirty = false
			s.coherenceMu.Unlock()
		}
	}()
}

// StartCoherenceSweeper runs the standing coherence check: once shortly after
// boot (past the boot pipeline), then on a slow ticker — so a book the repair
// lands at 4am is flagged loud within the interval without anyone remembering to
// look. The rescan path also sweeps immediately (handleLibraryRescan), so a
// manual "rescan now" gets an instant verdict; this ticker covers watcher-landed
// books that never pass through a rescan.
func (s *Server) StartCoherenceSweeper() {
	go func() {
		time.Sleep(90 * time.Second) // let the boot pipeline settle first
		s.sweepCoherence("boot")
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.sweepCoherence("periodic")
		}
	}()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
