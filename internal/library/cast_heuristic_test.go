package library

import (
	"strings"
	"testing"
)

import "github.com/pj/abookify/internal/db"

// Synthetic passage exercising the core signals: a dictionary-word name (Rose),
// a tightly-bound multi-word name (Van Helsing), a sentence-initial common word
// that must NOT be a character (The/Then), and frequency ordering.
func TestExtractCastHeuristic(t *testing.T) {
	text := `Van Helsing greeted Rose warmly. He quietly told Rose the truth, and Rose
thanked Van Helsing. Everyone admired Rose. Van Helsing trusted Rose deeply.
The night was cold, but Rose stayed. Later, Van Helsing and Rose talked at length.
Then Van Helsing smiled at Rose, and Rose smiled back at Van Helsing.`
	cast := ExtractCastHeuristic([]db.Chapter{{Content: text}}, 3)

	names := map[string]int{}
	for _, c := range cast {
		names[c.Name] = c.Mentions
	}
	if _, ok := names["Van Helsing"]; !ok {
		t.Errorf("expected merged 'Van Helsing' entity, got %v", names)
	}
	if _, ok := names["Van"]; ok {
		t.Errorf("'Van' should be consumed into 'Van Helsing', not standalone: %v", names)
	}
	if _, ok := names["Rose"]; !ok {
		t.Errorf("dictionary-word name 'Rose' should be detected (title-case mid-sentence): %v", names)
	}
	if _, ok := names["The"]; ok {
		t.Errorf("'The' is a sentence-initial stopword, must not be a character: %v", names)
	}
	if _, ok := names["Then"]; ok {
		t.Errorf("'Then' is only ever sentence-initial, must not be a character: %v", names)
	}
	if len(cast) > 0 && cast[0].Name != "Van Helsing" && cast[0].Name != "Rose" {
		t.Errorf("top character should be Van Helsing or Rose, got %q", cast[0].Name)
	}
}

func TestExtractCastHeuristic_Honorifics(t *testing.T) {
	// A title's trailing period must not read as a full stop. Characters who are
	// essentially always introduced by title (Frankenstein's Mr. Kirwin,
	// M. Waldman, M. Krempe) otherwise look sentence-initial every single time,
	// earn no mid-sentence mentions, and vanish from the ranking entirely.
	body := strings.Repeat(
		"The magistrate Mr. Kirwin entered the room. Later M. Waldman spoke to him about it. "+
			"I thanked Mr. Kirwin warmly and M. Waldman agreed with me. ", 4)
	chapters := []db.Chapter{{Content: body}}

	got := ExtractCastHeuristic(chapters, 3)
	byName := map[string]CastMember{}
	for _, c := range got {
		byName[c.Name] = c
	}

	for _, want := range []string{"Mr. Kirwin", "M. Waldman"} {
		c, ok := byName[want]
		if !ok {
			var names []string
			for _, g := range got {
				names = append(names, g.Name)
			}
			t.Fatalf("%q missing from cast; got %v", want, names)
		}
		if c.Mentions < 3 {
			t.Errorf("%q: mentions=%d, want >=3 (the title's dot suppressed them)", want, c.Mentions)
		}
	}

	// The honorific itself must never become a character.
	for _, bad := range []string{"Mr", "M", "Mr.", "M."} {
		if _, ok := byName[bad]; ok {
			t.Errorf("honorific %q leaked into the cast", bad)
		}
	}
}

func TestExtractCastHeuristic_SentenceEndStillEndsSentence(t *testing.T) {
	// The honorific carve-out must not disable real sentence detection: a
	// capitalized word that only ever starts sentences is not a name.
	body := strings.Repeat("Walked home slowly. Suddenly it rained hard. Nobody came. ", 8)
	for _, c := range ExtractCastHeuristic([]db.Chapter{{Content: body}}, 3) {
		if c.Name == "Suddenly" || c.Name == "Nobody" || c.Name == "Walked" {
			t.Errorf("sentence-initial word %q counted as a character", c.Name)
		}
	}
}
