package library

import "testing"

// TestIsPureRepetition guards the citation/context fix for the Hitchhiker's
// running-header garbage: a chunk that is one phrase repeated must be flagged
// (so it's dropped from retrieval), while genuine prose must not be.
func TestIsPureRepetition(t *testing.T) {
	junk := "HH1 - Hitchhiker's Guide to the Galaxy HH1 - Hitchhiker's Guide to the Galaxy"
	if !isPureRepetition(junk) {
		t.Fatalf("repeated running-header should be flagged as pure repetition")
	}
	// 3x repeat too
	if !isPureRepetition("Chapter One Chapter One Chapter One") {
		t.Fatalf("3x repeat should be flagged")
	}
	// Genuine prose — title prefix then real content — must NOT be flagged.
	real := "HH1 - Hitchhiker's Guide to the Galaxy Far out in the uncharted backwaters of the unfashionable end of the western spiral arm of the Galaxy lies a small unregarded yellow sun."
	if isPureRepetition(real) {
		t.Fatalf("prose with a title prefix must not be flagged")
	}
	// Ordinary sentence — not flagged.
	if isPureRepetition("The quick brown fox jumps over the lazy dog again and again today") {
		t.Fatalf("ordinary sentence must not be flagged")
	}
	// Too short to judge → not flagged.
	if isPureRepetition("hello hello") {
		t.Fatalf("very short repeats are below the confidence floor")
	}
}
