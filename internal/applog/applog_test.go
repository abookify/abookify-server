package applog

import "testing"

func TestClassifyLine(t *testing.T) {
	cases := []struct {
		msg   string
		comp  string
		level Level
	}{
		// The motivating bug: "warning: … failed …" has both "warn" and
		// "fail" — the leading prefix must win → WARN, not ERROR.
		{"warning: embedded chapter probe failed for 30.mp3: ffprobe: exit status 1", "system", LevelWarn},
		{"warn: something", "system", LevelWarn},
		// Genuine errors with no warning prefix stay ERROR.
		{"embed: book 12 (X): embed batch failed: 500", "system", LevelError},
		{"error: connection refused", "system", LevelError},
		{"panic: nil deref", "system", LevelError},
		{"fatal: boom", "system", LevelError},
		// Keyword scan still catches mid-line error/fail when no prefix.
		{"something went wrong: request failed", "system", LevelError},
		// ACCESS lines route to http/debug.
		{"ACCESS ip=1.2.3.4 GET /api/health 200", "http", LevelDebug},
		// Plain info.
		{"abookify server starting", "system", LevelInfo},
	}
	for _, c := range cases {
		comp, lvl := classifyLine(c.msg)
		if comp != c.comp || lvl != c.level {
			t.Errorf("classifyLine(%q) = (%s,%s), want (%s,%s)", c.msg, comp, lvl, c.comp, c.level)
		}
	}
}
