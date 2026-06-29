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
			toks := Tokenize(out)            // the alignment tokenizer
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
