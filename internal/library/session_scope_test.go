package library

import "testing"

// #130: the per-chat scope resolves to a server-enforced retrieval bound.
func TestResolveSessionScope(t *testing.T) {
	// reading mode → up_to the reader's current chapter (spoiler-safe).
	got := ResolveSessionScope("reading", 7, 4, QueryScope{})
	if got.Type != "up_to_chapter" || got.BookID != 7 || got.ChapterIdx != 4 {
		t.Errorf("reading → %+v, want up_to_chapter book 7 ch 4", got)
	}
	// book mode → whole book.
	if got := ResolveSessionScope("book", 7, 4, QueryScope{}); got.Type != "book" {
		t.Errorf("book → %+v, want book", got)
	}
	// reading mode but no reader position → falls back to whole book.
	if got := ResolveSessionScope("reading", 0, -1, QueryScope{}); got.Type != "book" {
		t.Errorf("reading w/o position → %+v, want book", got)
	}
	// an explicit paragraph override narrows regardless of mode.
	ov := QueryScope{Type: "paragraph", BookID: 7, ChapterIdx: 2, ParagraphIdx: 3}
	if got := ResolveSessionScope("book", 7, 9, ov); got.Type != "paragraph" || got.ParagraphIdx != 3 {
		t.Errorf("override → %+v, want the paragraph override", got)
	}
	// empty/unknown mode defaults to spoiler-safe reading behavior.
	if got := ResolveSessionScope("", 7, 4, QueryScope{}); got.Type != "up_to_chapter" {
		t.Errorf("default mode → %+v, want up_to_chapter (spoiler-safe)", got)
	}
}
