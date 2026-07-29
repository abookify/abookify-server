package library

import "testing"

// The silence path trusts two very different things: a chapter-grade SILENCE is
// acoustic ground truth, while the NUMBER is whatever Whisper heard. Whisper
// mishears — the Norm excerpt produced two "Chapter 22" — and the narrator path's
// monotonic guard never applied here. Boundaries must survive; numbers get
// repaired.
func TestReconcileSilenceChapterNumbers(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "duplicate spoken number is renumbered, boundary kept",
			in:   []string{"Chapter 20", "Chapter 21", "Chapter 22", "Chapter 22"},
			want: []string{"Chapter 20", "Chapter 21", "Chapter 22", "Chapter 23"},
		},
		{
			// A backwards number is NOT necessarily wrong: the silence path
			// emits positional fallbacks for un-announced boundaries, which look
			// identical to spoken numbers. Only a genuine collision is repaired.
			name: "a backwards but unused number is left alone",
			in:   []string{"Chapter 20", "Chapter 4", "Chapter 21"},
			want: []string{"Chapter 20", "Chapter 4", "Chapter 21"},
		},
		{
			name: "duplicate is moved above everything seen, not to prev+1",
			in:   []string{"Chapter 20", "Chapter 4", "Chapter 21", "Chapter 21"},
			want: []string{"Chapter 20", "Chapter 4", "Chapter 21", "Chapter 22"},
		},
		{
			name: "subtitle is preserved when renumbering",
			in:   []string{"Chapter 22: The Devil, You Say", "Chapter 22: Night Falls"},
			want: []string{"Chapter 22: The Devil, You Say", "Chapter 23: Night Falls"},
		},
		{
			name: "a clean ascending run is left completely alone",
			in:   []string{"Chapter 1", "Chapter 2", "Chapter 3"},
			want: []string{"Chapter 1", "Chapter 2", "Chapter 3"},
		},
		{
			name: "gaps in a genuine numbering are respected, not compacted",
			in:   []string{"Chapter 1", "Chapter 5", "Chapter 9"},
			want: []string{"Chapter 1", "Chapter 5", "Chapter 9"},
		},
		{
			name: "Part and Book keywords are handled too",
			in:   []string{"Part 2", "Part 2"},
			want: []string{"Part 2", "Part 3"},
		},
		{
			name: "untitled chapters are untouched and do not consume numbers",
			in:   []string{"Chapter 1", "The Wind Rises", "Chapter 2"},
			want: []string{"Chapter 1", "The Wind Rises", "Chapter 2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var chapters []sttChapter
			for i, title := range tc.in {
				chapters = append(chapters, sttChapter{Title: title, WordIdx: i * 100})
			}
			got := reconcileSilenceChapterNumbers(chapters)
			if len(got) != len(tc.in) {
				t.Fatalf("chapter COUNT changed %d -> %d; boundaries must never be dropped",
					len(tc.in), len(got))
			}
			for i := range got {
				if got[i].Title != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i].Title, tc.want[i])
				}
				if got[i].WordIdx != i*100 {
					t.Errorf("[%d] boundary moved: WordIdx %d", i, got[i].WordIdx)
				}
			}
		})
	}
}
