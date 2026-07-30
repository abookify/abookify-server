package library

import (
	"strings"
	"testing"
)

// A Calibre-converted chapter puts the same line in <head><title> and in <h1>.
// Stripping tags without dropping <head> leaves the title string behind, so it
// lands at the top of the chapter and duplicates the heading.
//
// This produced 38 chunks on Hitchhiker's Guide that were nothing but the title
// twice over, all embedded, all retrievable as Q&A citations — PJ saw one cited
// on his phone instead of book text.
func TestHTMLToTextDropsHeadTitle(t *testing.T) {
	raw := `<?xml version='1.0' encoding='utf-8'?>
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
  <title>HH1 - Hitchhiker's Guide to the Galaxy</title>
  <meta name="DC.Title" content="HH1 - Hitchhiker's Guide to the Galaxy"/>
  <link href="../stylesheet.css" rel="stylesheet"/>
</head>
<body class="calibre3">
  <div><h1 class="calibre5">HH1 - Hitchhiker's Guide to the Galaxy</h1></div>
  <p>Far out in the uncharted backwaters of the unfashionable end of the western spiral arm.</p>
</body></html>`

	got := htmlToText(raw)
	if n := strings.Count(got, "Hitchhiker"); n != 1 {
		t.Errorf("title appears %d times, want 1 (the <h1>); <head> leaked\n%s", n, got)
	}
	if !strings.Contains(got, "Far out in the uncharted") {
		t.Errorf("body prose lost:\n%s", got)
	}
	if strings.Contains(got, "stylesheet") {
		t.Errorf("head link text leaked:\n%s", got)
	}
}

// A chapter with no <head> must be unaffected.
func TestHTMLToTextWithoutHead(t *testing.T) {
	got := htmlToText(`<body><h1>Chapter I</h1><p>It was a bright cold day in April.</p></body>`)
	if !strings.Contains(got, "Chapter I") || !strings.Contains(got, "bright cold day") {
		t.Errorf("content lost when there is no head: %q", got)
	}
}

// Calibre stamps the BOOK title as an <h1> in every split document, so dozens of
// "chapters" contain that line and nothing else. Hitchhiker's Guide yielded 36
// such chapters out of 72; each became an embedded chunk that Q&A cited as though
// it were book text.
func TestIsHeadingOnly(t *testing.T) {
	title := "HH1 - Hitchhiker's Guide to the Galaxy"
	cases := []struct {
		text, title string
		want        bool
		why         string
	}{
		{title, title, true, "content is exactly the heading"},
		{title + " " + title, title, true, "heading repeated, still no body"},
		{title + " Far out in the uncharted backwaters of the unfashionable end.", title, false,
			"heading plus real prose is a real chapter"},
		{"CHAPTER 1 The house stood on a slight rise just on the edge of the village.", "CHAPTER 1", false,
			"ordinary chapter"},
		{"Hitchhiker’s Guide to the Galaxy", "Hitchhiker's Guide to the Galaxy", true,
			"curly vs straight apostrophe must still match"},
		{"", "", true, "empty is heading-only"},
		{"A short but genuine opening paragraph of prose here.", "", false,
			"no title known, but there is body text"},
	}
	for _, c := range cases {
		if got := isHeadingOnly(c.text, c.title); got != c.want {
			t.Errorf("%s: isHeadingOnly(%q, %q) = %v, want %v", c.why, c.text, c.title, got, c.want)
		}
	}
}
