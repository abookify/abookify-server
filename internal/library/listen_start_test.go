package library

import (
	"testing"

	"github.com/pj/abookify/internal/db"
)

func audio(id int64, title string, dur float64) db.Book {
	return db.Book{ID: id, MediaType: "audio", Origin: "tts_kokoro", Title: title, Duration: dur}
}

func TestListenStart_SkipsFrontMatter(t *testing.T) {
	cases := []struct {
		name  string
		title string
		files []db.Book
		want  int64
	}{
		{"dracula: title page, stub, then chapter I titled with the book name", "Dracula",
			[]db.Book{audio(1, "D R A C U L A", 26), audio(2, "Chapter 3", 29), audio(3, "D R A C U L A", 1814), audio(4, "CHAPTER II", 1706)}, 3},
		{"pride and prejudice: a 30-minute front matter titled with the book name", "Pride and Prejudice",
			[]db.Book{audio(1, "PRIDE.\nand\nPREJUDICE", 1808), audio(2, "Chapter I.", 1047)}, 2},
		{"carol: 95 s of title + preface", "A Christmas Carol",
			[]db.Book{audio(1, "A CHRISTMAS CAROL", 95), audio(2, "STAVE  ONE.", 2049)}, 2},
		{"sherlock: 58 s title page", "The Adventures of Sherlock Holmes",
			[]db.Book{audio(1, "The Adventures of Sherlock Holmes", 58), audio(2, "I.\nA SCANDAL IN BOHEMIA", 2656)}, 2},
		{"single long file: nothing to skip", "438 Days",
			[]db.Book{audio(1, "438 Days", 30000)}, 1},
		{"all short: fall back to the first", "Tiny",
			[]db.Book{audio(1, "Tiny", 20), audio(2, "More", 30)}, 1},
		{"human narration whose first file is a real chapter", "Dracula",
			[]db.Book{{ID: 9, MediaType: "audio", Origin: "narrator_recording", Title: "01-Jonathan Harker’s Journal", Duration: 1817}, {ID: 10, MediaType: "audio", Origin: "narrator_recording", Title: "02-Jonathan Harker’s Journal", Duration: 1700}}, 9},
	}
	for _, c := range cases {
		w := &db.Work{Title: c.title, AudioFiles: c.files}
		got := ListenStart(w)
		if got == nil || got.ID != c.want {
			id := int64(0)
			if got != nil {
				id = got.ID
			}
			t.Errorf("%s: got book %d, want %d", c.name, id, c.want)
		}
	}
}

func TestNormTitle_LetterSpaced(t *testing.T) {
	if normTitle("D R A C U L A") != "dracula" {
		t.Errorf("letter-spaced title not collapsed: %q", normTitle("D R A C U L A"))
	}
}
