package server

import (
	"reflect"
	"testing"
)

// changedWorkIDs is the heart of the event-driven coherence watcher: given the
// previous and current content_version snapshots, it must surface exactly the
// works a repair touched (new or bumped), ignore untouched and deleted ones, and
// stay writer-agnostic — two repairs landing books "at once" appear as multiple
// changed ids in one diff, which is precisely the two-writer (tank + atrium) case.
func TestChangedWorkIDs(t *testing.T) {
	cases := []struct {
		name string
		prev map[int64]string
		cur  map[int64]string
		want []int64
	}{
		{
			name: "no change",
			prev: map[int64]string{1: "a", 2: "b"},
			cur:  map[int64]string{1: "a", 2: "b"},
			want: nil,
		},
		{
			name: "one repaired (content_version moved)",
			prev: map[int64]string{1: "2026-07-29", 2: "b"},
			cur:  map[int64]string{1: "2026-08-06", 2: "b"},
			want: []int64{1},
		},
		{
			name: "new work appears",
			prev: map[int64]string{1: "a"},
			cur:  map[int64]string{1: "a", 2: "b"},
			want: []int64{2},
		},
		{
			name: "new work with empty content_version still counts (presence, not value)",
			prev: map[int64]string{1: "a"},
			cur:  map[int64]string{1: "a", 2: ""},
			want: []int64{2},
		},
		{
			name: "two writers land books in the same interval — both surfaced, sorted",
			prev: map[int64]string{5: "old", 9: "old", 40: "same"},
			cur:  map[int64]string{5: "new", 9: "new", 40: "same"},
			want: []int64{5, 9},
		},
		{
			name: "deletion is ignored (no coherence check needed)",
			prev: map[int64]string{1: "a", 2: "b"},
			cur:  map[int64]string{1: "a"},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := changedWorkIDs(c.prev, c.cur)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("changedWorkIDs() = %v, want %v", got, c.want)
			}
		})
	}
}
