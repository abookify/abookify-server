package library

import (
	"strings"
	"testing"
)

// htmlToText must not leak HTML-entity or footnote-marker artifacts into the
// plain-text/alignment content — otherwise they show as FALSE diffs in the meld
// (server-web follow-up: 'nbsp', 'four1', 'mizzen mast bc').
func TestHtmlToText_EntitiesAndFootnoteArtifacts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// substrings that must NOT appear as tokens, and ones that must.
		absent  []string
		present []string
	}{
		{
			name:    "nbsp entity is not a literal token",
			in:      `<p>Chocolat&nbsp;&nbsp;&nbsp;ONE&nbsp;February&nbsp;11</p>`,
			absent:  []string{"nbsp", "&nbsp"},
			present: []string{"Chocolat", "ONE", "February", "11"},
		},
		{
			name:    "superscript footnote marker detached from word",
			in:      `<p>the number four<sup>1</sup> and the mizzen-mast<sup>bc</sup> creaked.</p>`,
			absent:  []string{"four1", "mizzenbc", "mast bc", "mastbc"},
			present: []string{"four", "creaked"},
		},
		{
			name:    "footnote noteref anchor content dropped",
			in:      `<p>He paused<a epub:type="noteref" href="#fn3">3</a> at the door<a href="#footnote7">7</a>.</p>`,
			absent:  []string{"paused3", "door7", "3", "7"},
			present: []string{"paused", "door"},
		},
		{
			name:    "named + numeric entities decode",
			in:      `<p>Tom &amp; Jerry said &#8220;hi&#8221; &mdash; nice.</p>`,
			absent:  []string{"amp", "8220", "8221", "mdash"},
			present: []string{"Tom", "Jerry", "nice"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := htmlToText(c.in)
			toks := Tokenize(out) // the alignment tokenizer
			tokset := map[string]bool{}
			for _, tk := range toks {
				tokset[tk] = true
			}
			for _, a := range c.absent {
				// absent as a whole token (after normalization) …
				if tokset[strings.ToLower(strings.Trim(a, "& "))] {
					t.Errorf("got false token %q in %q (text=%q)", a, toks, out)
				}
				// … and absent as a substring of the raw text where it'd be glued.
				if strings.Contains(strings.ToLower(out), strings.ToLower(a)) && !strings.Contains(a, " ") {
					t.Errorf("artifact %q survived in text %q", a, out)
				}
			}
			for _, p := range c.present {
				if !tokset[strings.ToLower(p)] {
					t.Errorf("expected token %q missing from %q (text=%q)", p, toks, out)
				}
			}
		})
	}
}

func TestTrimGutenbergBoilerplate(t *testing.T) {
	// Shape taken from a real PG epub (#75011): sentinels sit in a <span>
	// inside pg-boilerplate divs, licence text runs to the end of the file.
	const doc = `<div id="pg-header"><p>Title: All Quiet</p><p>Release date: 2025</p>
<div id="pg-start-separator">
<span>*** START OF THE PROJECT GUTENBERG EBOOK ALL QUIET ON THE WESTERN FRONT ***</span>
</div></div>
<h2>CHAPTER I</h2><p>We are at rest five miles behind the front.</p>
<h2>CHAPTER XII</h2><p>It is autumn.</p>
<div class="pg-boilerplate pgheader footer" id="pg-footer">
<div id="pg-end-separator">
<span>*** END OF THE PROJECT GUTENBERG EBOOK ALL QUIET ON THE WESTERN FRONT ***</span>
</div>
<p>Updated editions will replace the previous one.</p>
<p>Project Gutenberg is a registered trademark, and may not be used if you charge for an eBook.</p></div>`

	got := trimGutenbergBoilerplate(doc)

	for _, leaked := range []string{"Release date", "registered trademark", "Updated editions"} {
		if strings.Contains(got, leaked) {
			t.Errorf("boilerplate %q survived the trim:\n%s", leaked, got)
		}
	}
	for _, kept := range []string{"CHAPTER I", "five miles behind the front", "CHAPTER XII", "It is autumn"} {
		if !strings.Contains(got, kept) {
			t.Errorf("book text %q was trimmed away:\n%s", kept, got)
		}
	}
}

func TestTrimGutenbergBoilerplate_NonGutenbergUntouched(t *testing.T) {
	// A publisher epub has no sentinels and must come back byte-identical —
	// the trim must never guess at where a non-PG book starts or ends.
	const doc = `<h1>Chapter One</h1><p>It was a bright cold day in April.</p>`
	if got := trimGutenbergBoilerplate(doc); got != doc {
		t.Errorf("non-PG document was modified:\ngot  %q\nwant %q", got, doc)
	}
}

func TestTrimGutenbergBoilerplate_ThisVariantAndOnlyFooter(t *testing.T) {
	// Older PG files say "THIS PROJECT GUTENBERG EBOOK"; and a spine file may
	// contain only the footer sentinel (extractPerSpineFile trims per file).
	const doc = `<p>The last line of the book.</p>
<span>*** END OF THIS PROJECT GUTENBERG EBOOK ALICE ***</span>
<p>Section 1. General Terms of Use.</p>`
	got := trimGutenbergBoilerplate(doc)
	if strings.Contains(got, "General Terms of Use") {
		t.Errorf("footer licence survived:\n%s", got)
	}
	if !strings.Contains(got, "The last line of the book.") {
		t.Errorf("book text was trimmed away:\n%s", got)
	}
}

func TestTrimGutenbergBoilerplate_PreFenceSignOff(t *testing.T) {
	// Pre-2020 PG files put a bare sign-off line BEFORE the fenced end marker,
	// so cutting only at the fence leaves it as the book's last words.
	const doc = `<p>They have a world to win. WORKING MEN OF ALL COUNTRIES, UNITE!</p>
<p>End of the Project Gutenberg EBook of The Communist Manifesto, by Karl Marx</p>
<span>*** END OF THIS PROJECT GUTENBERG EBOOK THE COMMUNIST MANIFESTO ***</span>
<p>Section 1. General Terms of Use.</p>`
	got := trimGutenbergBoilerplate(doc)
	if strings.Contains(got, "End of the Project Gutenberg") {
		t.Errorf("bare sign-off survived:\n%s", got)
	}
	if !strings.Contains(got, "WORKING MEN OF ALL COUNTRIES, UNITE!") {
		t.Errorf("book text was trimmed away:\n%s", got)
	}
}

// A closing </p> must yield a paragraph break (blank line), distinct from
// <br>/hard-wraps — the one-character defect that flattened all 53 library
// epubs (2026-08-10).
func TestExtractPreservesParagraphBreaks(t *testing.T) {
	html := `<html><body><p>Marley was dead:
to begin with.</p><p>There is no doubt<br/>whatever about that.</p></body></html>`
	text := htmlToText(html)
	if !strings.Contains(text, "\n\n") {
		t.Fatalf("no paragraph break survived extraction: %q", text)
	}
	paras := strings.Split(text, "\n\n")
	if len(paras) != 2 {
		t.Fatalf("want 2 paragraphs, got %d: %q", len(paras), text)
	}
	if strings.Contains(paras[0], "\n") {
		t.Errorf("intra-paragraph wrap should be healed to a space: %q", paras[0])
	}
}

// Some publisher EPUBs use <div> as the paragraph element (Vintage's Gulag
// Archipelago, Recorded Books' Crime and Punishment). The whitelist dropped
// <div> silently, so content_html became one run of <span>s and the reader
// rendered the whole chapter as one block (board 11). <div> is now a <p>, and
// the wrapper nesting that leaves is folded to one level.
func TestSanitizeHTMLTreatsDivAsParagraph(t *testing.T) {
	raw := `<body><div class="body"><div class="para">Once <span>we</span> have taken up the word.</div>
<div class="para">A writer is no detached judge.</div><div class="empty"> </div></div></body>`
	got := sanitizeHTML(raw)
	if n := strings.Count(got, "<p>"); n != 2 {
		t.Fatalf("want 2 paragraphs, got %d in %q", n, got)
	}
	if strings.Contains(got, "<p><p>") || strings.Contains(got, "</p></p>") || strings.Contains(got, "<p></p>") {
		t.Errorf("wrapper nesting not folded: %q", got)
	}
	if !strings.Contains(got, "<p>Once <span>we</span> have taken up the word.</p>") {
		t.Errorf("paragraph text/inline markup lost: %q", got)
	}
	// Real <p> documents are untouched by the rewrite.
	if got := sanitizeHTML(`<p>One.</p><p>Two.</p>`); got != `<p>One.</p><p>Two.</p>` {
		t.Errorf("plain <p> document changed: %q", got)
	}
}
