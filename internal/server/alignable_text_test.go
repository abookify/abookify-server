package server

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// TestHasAlignableText locks the discriminator behind the coverage podcast case:
// "something to align AGAINST" means a NON-transcript text source (epub/txt/pdf),
// NOT has_text — a whisper transcript is a text book but IS the audio.
func TestHasAlignableText(t *testing.T) {
	cases := []struct {
		name string
		text []db.Book
		want bool
	}{
		{"podcast — audio only, no text", nil, false},
		{"podcast — transcript only (has_text true, but IS the audio)",
			[]db.Book{{Format: "transcript", MediaType: "text"}}, false},
		{"has an epub to align against",
			[]db.Book{{Format: "epub", MediaType: "text"}}, true},
		{"txt counts", []db.Book{{Format: "txt", MediaType: "text"}}, true},
		{"pdf counts", []db.Book{{Format: "pdf", MediaType: "text"}}, true},
		{"transcript + epub → epub wins",
			[]db.Book{{Format: "transcript"}, {Format: "epub"}}, true},
		{"internal-visibility epub does NOT count",
			[]db.Book{{Format: "epub", Visibility: "internal"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasAlignableText(db.Work{TextFiles: c.text})
			if got != c.want {
				t.Errorf("hasAlignableText(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
