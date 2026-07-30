package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// A running header repeated across most chapters is removed everywhere, while a
// per-chapter-distinct heading (a real chapter title) is left untouched.
func TestStripRunningHeaders_RemovesRepeatedHeaderKeepsTitles(t *testing.T) {
	const hdr = "HH1 - Hitchhiker's Guide to the Galaxy"
	mk := func(idx int, title, body string) db.Chapter {
		text := hdr + "\n" + title + "\n" + body
		return db.Chapter{Index: idx, Title: title, Content: text,
			ContentHTML: "<p>" + hdr + "</p><h2>" + title + "</h2><p>" + body + "</p>",
			WordCount:   len(strings.Fields(text))}
	}
	in := []db.Chapter{
		mk(0, "Chapter 1", "The house stood on a slight rise just on the edge of the village."),
		mk(1, "Chapter 2", "Arthur woke to the sound of a bulldozer engine outside his window."),
		mk(2, "Chapter 3", "Far out in the uncharted backwaters of the unfashionable end."),
		mk(3, "Chapter 4", "The ships hung in the sky in much the same way that bricks don't."),
	}
	out := stripRunningHeaders(in)

	if len(out) != 4 {
		t.Fatalf("chapter count changed: got %d want 4", len(out))
	}
	for i, ch := range out {
		if strings.Contains(ch.Content, "HH1") {
			t.Errorf("ch %d: running header survived in Content: %q", i, ch.Content)
		}
		if strings.Contains(ch.ContentHTML, "HH1") {
			t.Errorf("ch %d: running header survived in ContentHTML: %q", i, ch.ContentHTML)
		}
		// The real chapter title MUST remain — it varies per chapter, so it must
		// never be mistaken for boilerplate.
		if !strings.Contains(ch.Content, ch.Title) {
			t.Errorf("ch %d: chapter title %q was stripped: %q", i, ch.Title, ch.Content)
		}
		if ch.WordCount != len(strings.Fields(ch.Content)) {
			t.Errorf("ch %d: WordCount %d not recomputed (content has %d)", i, ch.WordCount, len(strings.Fields(ch.Content)))
		}
	}
}

// Below the chapter-count floor, nothing is stripped even if a line repeats —
// short books are where incidental repeats would be false positives.
func TestStripRunningHeaders_ShortBookUntouched(t *testing.T) {
	in := []db.Chapter{
		{Index: 0, Title: "I", Content: "Ditty\nfirst chapter body text here"},
		{Index: 1, Title: "II", Content: "Ditty\nsecond chapter body text here"},
	}
	out := stripRunningHeaders(in)
	for i, ch := range out {
		if !strings.Contains(ch.Content, "Ditty") {
			t.Errorf("ch %d: short-book line wrongly stripped: %q", i, ch.Content)
		}
	}
}

// A long recurring line (real prose that happens to repeat, e.g. a refrain) is
// NOT treated as a running header — headers are short.
func TestStripRunningHeaders_LongLineNotHeader(t *testing.T) {
	refrain := "and so it was that the travellers pressed on through the long cold night without rest"
	var in []db.Chapter
	for i := 0; i < 6; i++ {
		in = append(in, db.Chapter{Index: i, Content: refrain + "\nunique body line " + strings.Repeat("x", i+1)})
	}
	out := stripRunningHeaders(in)
	for i, ch := range out {
		if !strings.Contains(ch.Content, "travellers pressed on") {
			t.Errorf("ch %d: long recurring line wrongly stripped as a header", i)
		}
	}
}

// Regression against the real Hitchhiker's EPUB: the "HH1 - …" running header is
// gone from every chapter's Content, and the book still has its chapters.
func TestExtractEPUB_HitchhikersNoRunningHeader(t *testing.T) {
	chs, err := ExtractEPUBChapters("../../testdata/library/ebooks/hitchhikers_guide.epub", 45010)
	if err != nil {
		t.Skipf("epub not available: %v", err)
	}
	if len(chs) < 10 {
		t.Fatalf("expected the full book, got %d chapters", len(chs))
	}
	for _, ch := range chs {
		if strings.Contains(ch.Content, "HH1") {
			t.Errorf("chapter %q still carries the HH1 running header: %q", ch.Title, firstNL(ch.Content))
		}
	}
}

func firstNL(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
