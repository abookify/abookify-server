package library

import (
	"strings"
	"testing"
)

// stripGutenbergApparatus must remove the two PG front-matter blocks the
// sentinel/running-head passes miss, while leaving the book's own title page,
// TOC and — critically — any real prose that merely mentions Gutenberg intact.
func TestStripGutenbergApparatus(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantGone   []string // substrings that MUST be removed
		wantKept   []string // substrings that MUST survive (must-not-strip)
		wantSameAs string   // if set, output must equal input exactly (nothing touched)
	}{
		{
			name: "editions listing stripped, title page kept (Christmas Carol form)",
			in: strings.Join([]string{
				"There are several editions of this ebook in the Project Gutenberg collection. Various characteristics of each ebook are listed.",
				"Click on any of the filenumbers below to quickly view each ebook.",
				"46 (Original First Edition Cover; 1843 Illustrations by John Leech)",
				"19337 (Published in 1905; Illustrations by G. A. Williams)",
				"Cover of 1843 First Edition",
				"A CHRISTMAS CAROL",
				"BY CHARLES DICKENS",
				"Marley was dead: to begin with.",
			}, "\n"),
			wantGone: []string{"several editions of this ebook", "Click on any of the filenumbers", "46 (Original First Edition", "19337 (Published"},
			wantKept: []string{"Cover of 1843 First Edition", "A CHRISTMAS CAROL", "BY CHARLES DICKENS", "Marley was dead"},
		},
		{
			name: "editor's note (label + sentence) stripped, TOC + prose kept (Meditations form)",
			in: strings.Join([]string{
				"This is a standard Windows font, so should be present on most systems.",
				"Project Gutenberg Editor's Note:",
				"The original html file with the passages in Greek in symbol.ttf font do not display in many browsers.",
				"BOOKS",
				"INTRODUCTION",
				"Of my grandfather Verus I have learned to be gentle.",
			}, "\n"),
			wantGone: []string{"Editor's Note", "passages in Greek in symbol.ttf"},
			wantKept: []string{"standard Windows font", "BOOKS", "INTRODUCTION", "Of my grandfather Verus"},
		},
		{
			name:       "MUST NOT STRIP: prose that names the printer Gutenberg",
			in:         "He set the type by hand, as Gutenberg had done four centuries before.\nThe press groaned.",
			wantSameAs: "He set the type by hand, as Gutenberg had done four centuries before.\nThe press groaned.",
		},
		{
			name:       "MUST NOT STRIP: a bare mention of Project Gutenberg volunteers in prose",
			in:         "This edition was lovingly produced by Project Gutenberg volunteers around the world.",
			wantSameAs: "This edition was lovingly produced by Project Gutenberg volunteers around the world.",
		},
		{
			name:       "MUST NOT STRIP: a numbered-parenthetical prose line outside any listing",
			in:         "1984 (a novel) changed how we talk about surveillance.\nHe read it twice.",
			wantSameAs: "1984 (a novel) changed how we talk about surveillance.\nHe read it twice.",
		},
		{
			name:       "MUST NOT STRIP: ordinary chapter prose untouched",
			in:         "It was the best of times, it was the worst of times.\nIt was the age of wisdom.",
			wantSameAs: "It was the best of times, it was the worst of times.\nIt was the age of wisdom.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripGutenbergApparatus(c.in)
			if c.wantSameAs != "" && got != c.wantSameAs {
				t.Fatalf("output changed but must be identical:\n  in:  %q\n  got: %q", c.wantSameAs, got)
			}
			for _, g := range c.wantGone {
				if strings.Contains(got, g) {
					t.Errorf("apparatus not removed — still contains %q\n  got: %q", g, got)
				}
			}
			for _, k := range c.wantKept {
				if !strings.Contains(got, k) {
					t.Errorf("real content wrongly removed — missing %q\n  got: %q", k, got)
				}
			}
		})
	}
}

// stripGutenbergApparatusHTML must drop the apparatus blocks from the reader HTML
// while leaving every prose block — including a block that merely mentions
// Gutenberg — untouched.
func TestStripGutenbergApparatusHTML(t *testing.T) {
	in := `<h4>There are several editions of this ebook in the Project Gutenberg collection. Click on any of the filenumbers below.</h4>` +
		`<p>Project Gutenberg Editor's Note: the Greek font may not render.</p>` +
		`<h1>A CHRISTMAS CAROL</h1>` +
		`<p>Marley was dead: to begin with.</p>` +
		`<p>He admired Gutenberg, who built the first printing press.</p>`
	got := stripGutenbergApparatusHTML(in)
	for _, gone := range []string{"several editions of this ebook", "Editor's Note"} {
		if strings.Contains(got, gone) {
			t.Errorf("apparatus block not removed: %q still present\n  got: %q", gone, got)
		}
	}
	for _, keep := range []string{"A CHRISTMAS CAROL", "Marley was dead", "He admired Gutenberg, who built the first printing press"} {
		if !strings.Contains(got, keep) {
			t.Errorf("prose block wrongly removed: %q missing\n  got: %q", keep, got)
		}
	}
}
