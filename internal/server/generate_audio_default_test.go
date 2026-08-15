package server

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

// The default narration source must never be a whisper transcript — narrating
// the STT of someone else's recording produces garbage (task 13). TextFiles is
// query-order and often lists the transcript first, so the plain "Generate
// audio" button (which sends no text_book_id) must still land on the real text.
func TestDefaultNarrationTextBookID(t *testing.T) {
	tx := db.Book{ID: 10, Origin: "whisper_transcript"}
	epub := db.Book{ID: 20, Origin: "publisher_epub"}
	epub2 := db.Book{ID: 30, Origin: "epub"}
	internalEpub := db.Book{ID: 40, Origin: "publisher_epub", Visibility: "internal"}

	cases := []struct {
		name string
		work db.Work
		want int64
	}{
		{"transcript-first paired work → the epub, not the transcript",
			db.Work{TextFiles: []db.Book{tx, epub}}, 20},
		{"display override to the epub is honored",
			db.Work{DisplayTextBookID: 30, TextFiles: []db.Book{tx, epub, epub2}}, 30},
		{"display override pointing at the TRANSCRIPT is ignored (still a real source)",
			db.Work{DisplayTextBookID: 10, TextFiles: []db.Book{tx, epub}}, 20},
		{"internal epubs are skipped for a visible real one",
			db.Work{TextFiles: []db.Book{tx, internalEpub, epub}}, 20},
		{"transcript-only work legitimately narrates its transcript",
			db.Work{TextFiles: []db.Book{tx}}, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultNarrationTextBookID(&c.work); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
