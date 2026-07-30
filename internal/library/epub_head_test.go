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
