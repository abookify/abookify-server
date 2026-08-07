package library

import (
	"strings"
	"testing"
)

// Gutenberg chapters embed their own heading; prepending the title again made
// the narrator open with "Stave One. Stave One. Marley's Ghost." — the first
// thirty seconds a starter-book listener hears.
func TestPreprocessSkipsDuplicateTitleAnnouncement(t *testing.T) {
	got := PreprocessForTTS("STAVE  ONE.", "STAVE ONE. MARLEY'S GHOST.\n\nMarley was dead: to begin with.")
	low := strings.ToLower(got)
	if strings.Count(low, "stave one") != 1 {
		t.Errorf("title announced twice:\n%s", got)
	}
	// A chapter whose body does NOT open with the title keeps the announcement.
	got = PreprocessForTTS("Chapter 2", "It was the best of times.")
	if !strings.HasPrefix(got, "Chapter 2") {
		t.Errorf("announcement lost for non-duplicated title: %q", got)
	}
}
