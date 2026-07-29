package library

import "testing"

func TestSanitizeCoverQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		// PJ's bug: the dash became a Solr NOT and returned zero results.
		{"Margaret Atwood - Handmaid's Tale", "Margaret Atwood Handmaid's Tale"},
		{"Handmaid's Tale", "Handmaid's Tale"},     // apostrophe kept
		{"Dune: Part One", "Dune Part One"},        // colon stripped
		{"  extra   spaces  ", "extra spaces"},     // whitespace collapsed
		{"C++ Primer", "C Primer"},                 // plus signs stripped + collapsed
		{"title (unabridged)", "title unabridged"}, // parens stripped
		{"*wildcard* ~fuzzy?", "wildcard fuzzy"},   // operators stripped
	}
	for _, c := range cases {
		if got := sanitizeCoverQuery(c.in); got != c.want {
			t.Errorf("sanitizeCoverQuery(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
