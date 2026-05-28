// Package applog is abookify's structured logging layer (#214). It
// gives every subsystem a single, greppable way to record what
// happened — successes, failures, and progress — with a level, a
// component, an optional job/work id, and arbitrary structured fields.
//
// Entries are echoed to stderr (so `docker logs` is unchanged) AND
// persisted to a recent ~24h window in SQLite, which the in-UI System
// Console (GET /api/logs) reads back. Persistence is asynchronous and
// best-effort: logging never blocks a hot path and never fails a
// request — if the buffer is full the entry is dropped, not awaited.
//
// This package is the format contract other sessions adopt: the
// transcription pipeline (stt/align) should call Log / the level
// wrappers with component "stt" / "align" and the relevant job id so
// its successes and failures land in the same console. The canonical
// entry point is Log; everything else is sugar over it.
package applog

import (
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Retention is how far back persisted logs are kept. The pruner trims
// older rows hourly so the table stays a bounded recent window.
const Retention = 24 * time.Hour

// Level is a log severity. Stored and serialized as its lowercase
// string ("debug"/"info"/"warn"/"error") so logs are greppable and the
// API needs no translation.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

var levelRank = map[Level]int{LevelDebug: 0, LevelInfo: 1, LevelWarn: 2, LevelError: 3}

// LevelsAtOrAbove returns the level strings with rank >= min, for
// building a min-level filter (used by the DB query layer). An unknown
// min is treated as debug (everything passes).
func LevelsAtOrAbove(min Level) []string {
	floor, ok := levelRank[min]
	if !ok {
		floor = 0
	}
	out := make([]string, 0, 4)
	for _, l := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		if levelRank[l] >= floor {
			out = append(out, string(l))
		}
	}
	return out
}

// Entry is one structured log record. JobID/WorkID/Fields are optional
// (zero values are omitted from JSON and stored empty). This is the
// shape persisted to the logs table and returned by GET /api/logs.
type Entry struct {
	Time      time.Time      `json:"time"`
	Level     Level          `json:"level"`
	Component string         `json:"component"`
	JobID     string         `json:"job_id,omitempty"`
	WorkID    int64          `json:"work_id,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// console renders an entry as a single human-readable line for stderr.
func (e Entry) console() string {
	var b strings.Builder
	b.WriteString(e.Time.Format("2006/01/02 15:04:05"))
	b.WriteString(" [")
	lvl := string(e.Level)
	if lvl == "" {
		lvl = "info"
	}
	b.WriteString(strings.ToUpper(lvl))
	b.WriteString("] ")
	if e.Component != "" {
		b.WriteString(e.Component)
		b.WriteByte(' ')
	}
	if e.JobID != "" {
		fmt.Fprintf(&b, "job=%s ", e.JobID)
	}
	if e.WorkID != 0 {
		fmt.Fprintf(&b, "work=%d ", e.WorkID)
	}
	b.WriteString(e.Message)
	for k, v := range e.Fields {
		fmt.Fprintf(&b, " %s=%v", k, v)
	}
	return b.String()
}

// Store is the persistence backend applog writes to. db.Store
// satisfies it. Kept as an interface so this package stays a leaf and
// callers can substitute a no-op in tests.
type Store interface {
	InsertLogs(entries []Entry) error
	PruneLogs(before time.Time) (int, error)
}

type sink struct {
	store   Store
	ch      chan Entry
	echo    io.Writer // original stderr — applog entries bypass the stdlib-capture tee
	dropped atomic.Int64
}

var std atomic.Pointer[sink]

// Init wires applog to a persistence store and starts the background
// drain + pruner goroutines. It also redirects the standard library
// `log` package through a tee so the ~200 existing log.Printf call
// sites are captured into the console as component "system" without
// touching them. Safe to call once at startup, after the DB is open.
func Init(store Store) {
	orig := log.Writer()
	s := &sink{
		store: store,
		ch:    make(chan Entry, 4096),
		echo:  orig,
	}
	std.Store(s)
	go s.run()
	go s.prune()

	// Tee stdlib log → original stderr (unchanged docker logs) + capture.
	log.SetOutput(io.MultiWriter(orig, captureWriter{}))
}

// run drains the buffer, batching inserts to keep DB churn low. Flushes
// when a batch fills or every ~750ms, whichever comes first.
func (s *sink) run() {
	batch := make([]Entry, 0, 256)
	t := time.NewTicker(750 * time.Millisecond)
	defer t.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		_ = s.store.InsertLogs(batch)
		batch = batch[:0]
	}
	for {
		select {
		case e := <-s.ch:
			batch = append(batch, e)
			if len(batch) >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// prune trims rows older than Retention. Runs shortly after boot, then
// hourly.
func (s *sink) prune() {
	time.Sleep(time.Minute)
	for {
		_, _ = s.store.PruneLogs(time.Now().Add(-Retention))
		time.Sleep(time.Hour)
	}
}

func (s *sink) emit(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	fmt.Fprintln(s.echo, e.console())
	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1) // buffer full — drop rather than block the caller
	}
}

// Log is the canonical structured entry point. component groups the
// subsystem ("jobs"/"stt"/"align"/"tts"/"server"/"scanner"/"system").
// jobID and workID are optional (pass "" / 0); fields is optional
// structured context (nil ok). Before Init the entry still reaches
// stderr, so early-boot logs aren't lost.
func Log(level Level, component, jobID string, workID int64, msg string, fields map[string]any) {
	e := Entry{
		Time: time.Now(), Level: level, Component: component,
		JobID: jobID, WorkID: workID, Message: msg, Fields: fields,
	}
	if s := std.Load(); s != nil {
		s.emit(e)
		return
	}
	fmt.Fprintln(log.Writer(), e.console())
}

// Level wrappers for the common case (no job/work id, no fields).
func Debug(component, msg string) { Log(LevelDebug, component, "", 0, msg, nil) }
func Info(component, msg string)  { Log(LevelInfo, component, "", 0, msg, nil) }
func Warn(component, msg string)  { Log(LevelWarn, component, "", 0, msg, nil) }
func Error(component, msg string) { Log(LevelError, component, "", 0, msg, nil) }

func Debugf(component, format string, a ...any) { Log(LevelDebug, component, "", 0, fmt.Sprintf(format, a...), nil) }
func Infof(component, format string, a ...any)  { Log(LevelInfo, component, "", 0, fmt.Sprintf(format, a...), nil) }
func Warnf(component, format string, a ...any)  { Log(LevelWarn, component, "", 0, fmt.Sprintf(format, a...), nil) }
func Errorf(component, format string, a ...any) { Log(LevelError, component, "", 0, fmt.Sprintf(format, a...), nil) }

// JobEvent records a job-scoped event under component "jobs". The
// generation queue uses it for status transitions; the transcription
// pipeline can use it (or Log with its own component) to attach a
// job id so its progress and failures join the same console.
func JobEvent(level Level, jobID string, workID int64, msg string, fields map[string]any) {
	Log(level, "jobs", jobID, workID, msg, fields)
}

// captureWriter receives raw lines from the stdlib logger. It only
// enqueues a "system" entry — the MultiWriter has already forwarded the
// bytes to stderr, so it must not echo again (that would double-print
// and risk recursion).
type captureWriter struct{}

var logPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

func (captureWriter) Write(p []byte) (int, error) {
	s := std.Load()
	if s == nil {
		return len(p), nil
	}
	msg := logPrefixRe.ReplaceAllString(strings.TrimRight(string(p), "\n"), "")
	comp, lvl := classifyLine(msg)
	select {
	case s.ch <- Entry{Time: time.Now(), Level: lvl, Component: comp, Message: msg}:
	default:
		s.dropped.Add(1)
	}
	return len(p), nil
}

// classifyLine assigns a component + level to a captured stdlib line by
// shape. Best-effort labeling for legacy call sites — new code uses the
// structured API directly and sets these precisely.
func classifyLine(msg string) (string, Level) {
	if strings.HasPrefix(msg, "ACCESS ") {
		return "http", LevelDebug
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "fatal"), strings.Contains(low, "panic"),
		strings.Contains(low, "error"), strings.Contains(low, "fail"):
		return "system", LevelError
	case strings.Contains(low, "warn"):
		return "system", LevelWarn
	default:
		return "system", LevelInfo
	}
}
