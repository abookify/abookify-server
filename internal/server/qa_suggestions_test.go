package server

import "testing"

// #131: the entity-chip spoiler bound is "does the name appear in the read
// text?". A character only mentioned later is structurally absent → withheld.
func TestNameAppearsIn(t *testing.T) {
	read := "i had known him a long while. marlow sat cross-legged. the nellie swung to her anchor."
	if !nameAppearsIn(read, "Charlie Marlow", nil) {
		t.Error("Marlow should be found via the last-name word")
	}
	if nameAppearsIn(read, "Kurtz", nil) {
		t.Error("Kurtz must NOT match read text that never mentions him (spoiler bound)")
	}
	if !nameAppearsIn(read, "Someone", []string{"Nellie"}) {
		t.Error("alias 'Nellie' should match")
	}
	// Short tokens (<4 chars) must not produce spurious matches.
	if nameAppearsIn("the cat sat", "Al", []string{"a"}) {
		t.Error("short tokens should be ignored")
	}
}

func TestShortCharName(t *testing.T) {
	if got := shortCharName("Józef Teodor Konrad Korzeniowski"); got != "Józef Korzeniowski" {
		t.Errorf("shortCharName = %q, want first+last", got)
	}
	if got := shortCharName("Mistah Kurtz"); got != "Mistah Kurtz" {
		t.Errorf("two-word name should be unchanged, got %q", got)
	}
}
