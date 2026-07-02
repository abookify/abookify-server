package server

import "testing"

// abookBaseName produces meaningful, filesystem-safe .abook names for the
// export set (was the generic work-<id>).
func TestAbookBaseName(t *testing.T) {
	cases := []struct {
		title, author string
		id            int64
		want          string
	}{
		{"Why We Sleep", "Matthew Walker", 22, "Why We Sleep - Matthew Walker"},
		{"Frankenstein", "", 28, "Frankenstein"},
		{"", "", 7, "work-7"}, // empty title → fallback
		{"  ", "", 9, "work-9"},
		{"Kill: Bill / Vol\"2", "Q*T", 3, "Kill Bill Vol 2 - Q T"}, // unsafe chars → space, collapsed
		{"Trailing dots...", "", 4, "Trailing dots"}, // trailing dots trimmed (Windows-safe)
	}
	for _, c := range cases {
		if got := abookBaseName(c.title, c.author, c.id); got != c.want {
			t.Errorf("abookBaseName(%q,%q,%d) = %q, want %q", c.title, c.author, c.id, got, c.want)
		}
	}
}
