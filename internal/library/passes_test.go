package library

import (
	"errors"
	"reflect"
	"testing"
)

// withTempPasses swaps in a clean registry for the test, restoring the
// global one after. Lets each test register passes without polluting
// the global state used in production code.
func withTempPasses(t *testing.T, fn func()) {
	t.Helper()
	saved := registeredPasses
	registeredPasses = map[string]*Pass{}
	defer func() { registeredPasses = saved }()
	fn()
}

func TestTopoSort_HonorsDependencies(t *testing.T) {
	withTempPasses(t, func() {
		Register(&Pass{Name: "chapters", Algo: "v1", Run: noop})
		Register(&Pass{Name: "paragraphs", Algo: "v1", DependsOn: []string{"chapters"}, Run: noop})
		Register(&Pass{Name: "summaries", Algo: "v1", DependsOn: []string{"chapters"}, Run: noop})
		ordered, err := topoSort([]string{"summaries", "paragraphs", "chapters"})
		if err != nil {
			t.Fatalf("topo: %v", err)
		}
		// chapters must precede both downstream passes
		idx := map[string]int{}
		for i, n := range ordered {
			idx[n] = i
		}
		if idx["chapters"] >= idx["paragraphs"] || idx["chapters"] >= idx["summaries"] {
			t.Errorf("ordering violates deps: %v", ordered)
		}
	})
}

func TestTopoSort_PullsInTransitiveDeps(t *testing.T) {
	withTempPasses(t, func() {
		Register(&Pass{Name: "chapters", Algo: "v1", Run: noop})
		Register(&Pass{Name: "summaries", Algo: "v1", DependsOn: []string{"chapters"}, Run: noop})
		// Asking only for summaries should still pull chapters in.
		ordered, err := topoSort([]string{"summaries"})
		if err != nil {
			t.Fatalf("topo: %v", err)
		}
		if !reflect.DeepEqual(ordered, []string{"chapters", "summaries"}) {
			t.Errorf("got %v, want [chapters summaries]", ordered)
		}
	})
}

func TestRunPasses_SkipsUpToDateAlgo(t *testing.T) {
	withTempPasses(t, func() {
		ranNames := []string{}
		Register(&Pass{
			Name: "chapters", Algo: "narrator@1.0",
			Run: func(ctx *PassContext) error {
				ranNames = append(ranNames, "chapters")
				return nil
			},
		})

		sc := &SidecarV3{
			Metadata: SidecarMetadata{
				Chapters: &ChapterSection{
					SectionMeta: SectionMeta{Algo: "narrator@1.0"},
				},
			},
		}
		ctx := &PassContext{Sidecar: sc}
		ran, err := RunPasses(ctx, []string{"chapters"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ran) != 0 || len(ranNames) != 0 {
			t.Errorf("up-to-date pass should have been skipped, ran=%v", ran)
		}
	})
}

func TestRunPasses_RecomputesOnAlgoBump(t *testing.T) {
	withTempPasses(t, func() {
		ranNames := []string{}
		Register(&Pass{
			Name: "chapters", Algo: "narrator@2.0", // newer than stored
			Run: func(ctx *PassContext) error {
				ranNames = append(ranNames, "chapters")
				return nil
			},
		})

		sc := &SidecarV3{
			Metadata: SidecarMetadata{
				Chapters: &ChapterSection{
					SectionMeta: SectionMeta{Algo: "narrator@1.0"}, // stale
				},
			},
		}
		ctx := &PassContext{Sidecar: sc}
		ran, _ := RunPasses(ctx, []string{"chapters"})
		if len(ran) != 1 || ran[0] != "chapters" {
			t.Errorf("expected chapters to rerun on algo mismatch, ran=%v", ran)
		}
	})
}

func TestRunPasses_ForceRunsEvenIfCurrent(t *testing.T) {
	withTempPasses(t, func() {
		Register(&Pass{
			Name: "chapters", Algo: "v1",
			Run: func(ctx *PassContext) error { return nil },
		})

		sc := &SidecarV3{
			Metadata: SidecarMetadata{
				Chapters: &ChapterSection{SectionMeta: SectionMeta{Algo: "v1"}},
			},
		}
		ctx := &PassContext{Sidecar: sc, Force: true}
		ran, err := RunPasses(ctx, []string{"chapters"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ran) != 1 {
			t.Errorf("Force=true should bypass algo-match, ran=%v", ran)
		}
	})
}

func TestRunPasses_PropagatesErrors(t *testing.T) {
	withTempPasses(t, func() {
		Register(&Pass{
			Name: "broken", Algo: "v1",
			Run: func(ctx *PassContext) error { return errors.New("oops") },
		})
		ctx := &PassContext{Sidecar: &SidecarV3{}}
		_, err := RunPasses(ctx, []string{"broken"})
		if err == nil || !containsStr(err.Error(), "broken") {
			t.Errorf("expected wrapped error, got %v", err)
		}
	})
}

func noop(ctx *PassContext) error { return nil }

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
