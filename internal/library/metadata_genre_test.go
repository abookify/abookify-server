package library

import "testing"

func TestPickGenre(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"Fiction"}, "Fiction"},
		{[]string{"FIC009000", "Fantasy fiction"}, "Fantasy fiction"}, // skip BISAC code
		{[]string{"  ", "Science Fiction"}, "Science Fiction"},        // skip blank
		{[]string{"Fiction / Fantasy"}, "Fiction / Fantasy"},          // keep compound
		{[]string{"FIC009000"}, ""},                                   // only a code → none
		{nil, ""},
	}
	for _, c := range cases {
		if got := pickGenre(c.in); got != c.want {
			t.Errorf("pickGenre(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
