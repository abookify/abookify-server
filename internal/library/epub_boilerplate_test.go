package library

import (
	"regexp"
	"strings"
	"testing"
)

// Regression for the "spurious trailing chapter" defect: a Project Gutenberg
// running-header line ("<Title> | Project Gutenberg") repeated atop every chapter
// could be promoted to its own short junk chapter (e.g. Sherlock rendered a
// "Chapter 14" with 8 words = the footer). stripRunningHeaders must drop it AND
// remove the header from real chapters' text. Asserts the extractor emits no
// short boilerplate chapter, and no real chapter body still leads with the
// header, across the Gutenberg EPUBs that carried this as stale DB rows — so the
// fix (commit 83a5b3b) can't silently regress.
func TestEPUBNoBoilerplateJunkChapter(t *testing.T) {
	boiler := regexp.MustCompile(`(?i)\bproject gutenberg\b`)
	files := []string{
		"alice-in-wonderland.epub", "jekyll-and-hyde.epub", "frankenstein.epub",
		"sleepy-hollow.epub", "sherlock-holmes.epub", "treasure-island-ai.epub",
	}
	for _, f := range files {
		f := f
		t.Run(f, func(t *testing.T) {
			chs, err := ExtractEPUBChapters("../../testdata/library/ebooks/"+f, 1)
			if err != nil {
				t.Skipf("epub not present / unreadable: %v", err)
			}
			for _, c := range chs {
				if c.WordCount < 25 && boiler.MatchString(c.Content) {
					t.Errorf("%s: junk boilerplate chapter survived: idx=%d words=%d content=%q",
						f, c.Index, c.WordCount, strings.TrimSpace(c.Content))
				}
				if boiler.MatchString(firstLine(c.Content)) {
					t.Errorf("%s: chapter idx=%d body still leads with the PG running header: %q",
						f, c.Index, firstLine(c.Content))
				}
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
