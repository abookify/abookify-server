package library

import "testing"

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
