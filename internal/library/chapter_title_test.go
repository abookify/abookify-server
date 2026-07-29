package library

import "testing"

// Real openings from 438 Days, where every one of fourteen chapters was named
// "Chapter N" while the narrator announced a usable title two words in.
func TestTitleFromAnnouncementRealBook(t *testing.T) {
	cases := []struct {
		content string
		want    string
		why     string
	}{
		{"Chapter 1. The Sharkers. His name was Salvador, and he was a fisherman.",
			"The Sharkers", "clean sentence-delimited title"},
		{"Chapter 3. Ambushed at Sea. November 18, 2012. Position 60 miles out.",
			"Ambushed at Sea", "title then a datestamp sentence"},
		{"Chapter 14. Who is this wild man? Help! Help! Help me!",
			"Who is this wild man", "question mark is a sentence boundary"},

		// Unpunctuated, but a spoken datestamp marks where the title ends.
		{"Chapter 4 Search and No Rescue November 19, 2012, Costa Azul",
			"Search and No Rescue", "datestamp bounds an unpunctuated title"},
		{"Chapter 5 Adrift November 23, 2012 Position, 280 miles out",
			"Adrift", "single-word title before a datestamp"},
		{"Chapter 6 Hunter-Gatherers November 30, 2012 550 miles",
			"Hunter-Gatherers", "hyphenated title before a datestamp"},

		// Datestamp INSIDE the first sentence: trim it, keep the title, rather
		// than rejecting the whole candidate for containing digits.
		{"Chapter 11. A Year at Sea November 3, 1911. November had come again.",
			"A Year at Sea", "title trimmed of a trailing datestamp"},

		// Still no way to tell where a title ends: recover nothing.
		{"Chapter Two A Stormy Tribe Salvador Alvarenga awoke to a grey dawn",
			"", "no delimiter and no datestamp — must not guess"},
	}
	for _, c := range cases {
		if got := TitleFromAnnouncement(c.content); got != c.want {
			t.Errorf("%s\n  content: %q\n  got %q, want %q", c.why, c.content, got, c.want)
		}
	}
}

func TestTitleFromAnnouncementRejects(t *testing.T) {
	cases := []struct {
		content, why string
	}{
		{"His name was Salvador, and he was a fisherman.", "no announcement at all"},
		{"Chapter 5. ", "announcement with nothing after it"},
		{"Chapter 5. Chapter 5. Adrift.", "second announcement, not a title"},
		{"Chapter 6. the boat drifted south for many days and nights on end.",
			"lowercase prose, not a title"},
		{"Chapter 7. A very long spoken heading that runs on well past any plausible title length.",
			"too long to be a title"},
		{"", "empty content"},
	}
	for _, c := range cases {
		if got := TitleFromAnnouncement(c.content); got != "" {
			t.Errorf("%s: got %q, want \"\"\n  content: %q", c.why, got, c.content)
		}
	}
}

// A month that is part of the title, with no date after it, must survive.
func TestTitleFromAnnouncementKeepsMonthInTitle(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Chapter 3. The Ides of March. Caesar fell that day.", "The Ides of March"},
		{"Chapter 4. A Cold December. The snow came early.", "A Cold December"},
	} {
		if got := TitleFromAnnouncement(c.in); got != c.want {
			t.Errorf("month in title was truncated: got %q, want %q for %q", got, c.want, c.in)
		}
	}
}

// Part/Book/Section announcements behave the same way.
func TestTitleFromAnnouncementOtherKinds(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Part Two. The Long Winter. Snow fell for weeks.", "The Long Winter"},
		{"Book III: The Return. He came home at last.", "The Return"},
		{"Section 4 — Aftermath. Nothing was the same.", "Aftermath"},
	} {
		if got := TitleFromAnnouncement(c.in); got != c.want {
			t.Errorf("got %q, want %q for %q", got, c.want, c.in)
		}
	}
}
