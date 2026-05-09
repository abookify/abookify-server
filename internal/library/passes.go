// Post-processing pass framework.
//
// A pass takes a sidecar + DB context and produces or refreshes one
// metadata section. Passes are individually rerunnable (drop a .redo
// file, hit the reprocess endpoint) and the runner walks them in
// dependency order so a downstream pass always sees fresh upstream data.
//
// The framework is intentionally simple: a registry of named passes,
// each declares what it depends on, the runner topologically orders them
// and skips any whose stored algo matches the current algo (unless
// `force` is set).
//
// Adding a new pass is one Register call + a Run function. No scheduler,
// no DAG library — the chain is short enough (handful of passes) that
// O(N^2) topo-sort is fine.
package library

import (
	"fmt"
	"log"
	"time"

	"github.com/pj/abookify/internal/db"
)

// Pass is one named post-processing step over a sidecar.
//
// Algo identifies the producer (e.g. "narrator+gap-fill@1.2"). When the
// runner sees a stored Algo on a metadata section that matches the
// pass's current Algo, the pass is skipped. Bumping Algo is the way to
// say "my output could change — recompute next time."
type Pass struct {
	// Name is the section identifier referenced by reprocess endpoints
	// and .redo files (e.g. "chapters", "paragraphs", "characters").
	Name string

	// Algo is the producer identifier stored on the section's SectionMeta.
	// Bump this whenever a code change could alter the output.
	Algo string

	// DependsOn lists pass names that must run before this one if any
	// of them are running on the same invocation. The runner uses this
	// to topo-sort the requested passes.
	DependsOn []string

	// Run executes the pass against the given context. It should write
	// its results to PassContext.Sidecar.Metadata.<Section> with the
	// matching Algo and ComputedAt fields filled in.
	Run func(ctx *PassContext) error
}

// PassContext is the bag of dependencies a pass receives. Keep it
// small — passes that need more should justify the addition.
type PassContext struct {
	Store        *db.Store
	WorkID       int64
	AudioBookID  int64
	TextBookID   int64 // the transcript "book" — created lazily by split_transcript
	Sidecar      *SidecarV3
	SidecarPath  string

	// Force makes the runner ignore the algo-match shortcut and run the
	// pass even if the stored output is current. Used by reprocess
	// endpoints where the user explicitly asked for a rerun.
	Force bool
}

// Registry holds all registered passes by name.
var registeredPasses = map[string]*Pass{}

// Register adds a pass to the global registry. Called from package init
// functions, one per pass file.
func Register(p *Pass) {
	if _, dup := registeredPasses[p.Name]; dup {
		panic(fmt.Sprintf("pass already registered: %q", p.Name))
	}
	registeredPasses[p.Name] = p
}

// passByName returns the registered pass or nil.
func passByName(name string) *Pass {
	return registeredPasses[name]
}

// allPassNames returns the set of registered pass names. Useful for
// "run everything" entry points.
func allPassNames() []string {
	names := make([]string, 0, len(registeredPasses))
	for n := range registeredPasses {
		names = append(names, n)
	}
	return names
}

// RunPasses runs the given pass names in dependency order. If `names`
// is empty, every registered pass is run. Skips passes whose stored
// Algo already matches the current Algo unless ctx.Force is true.
//
// Returns the list of passes that actually ran (algo-skips don't count).
func RunPasses(ctx *PassContext, names []string) ([]string, error) {
	if len(names) == 0 {
		names = allPassNames()
	}

	ordered, err := topoSort(names)
	if err != nil {
		return nil, err
	}

	ran := []string{}
	for _, n := range ordered {
		p := passByName(n)
		if p == nil {
			return ran, fmt.Errorf("unknown pass %q", n)
		}
		if !ctx.Force && sectionUpToDate(ctx.Sidecar, p) {
			continue
		}
		log.Printf("pass: running %s (algo: %s)", p.Name, p.Algo)
		if err := p.Run(ctx); err != nil {
			return ran, fmt.Errorf("pass %s: %w", p.Name, err)
		}
		ran = append(ran, p.Name)
	}
	return ran, nil
}

// topoSort orders the requested passes by their DependsOn relationships,
// implicitly pulling in transitive dependencies even if the caller only
// asked for a leaf pass. Cycles cause an error (none expected in our
// graph today, but the check protects against future regressions).
func topoSort(requested []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range requested {
		want[n] = true
	}
	// Pull in transitive deps so partial requests still produce a valid
	// chain (asking for "summaries" implicitly needs "chapters").
	frontier := append([]string(nil), requested...)
	for len(frontier) > 0 {
		n := frontier[0]
		frontier = frontier[1:]
		p := passByName(n)
		if p == nil {
			continue
		}
		for _, dep := range p.DependsOn {
			if !want[dep] {
				want[dep] = true
				frontier = append(frontier, dep)
			}
		}
	}

	// Kahn's algorithm. In-degree counted only over edges between
	// passes inside `want`.
	indeg := map[string]int{}
	for n := range want {
		indeg[n] = 0
	}
	for n := range want {
		p := passByName(n)
		if p == nil {
			continue
		}
		for _, dep := range p.DependsOn {
			if want[dep] {
				indeg[n]++
			}
		}
	}
	var queue, out []string
	for n, d := range indeg {
		if d == 0 {
			queue = append(queue, n)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		out = append(out, n)
		// Decrement in-degree of every pass that depends on n.
		for m := range want {
			p := passByName(m)
			if p == nil {
				continue
			}
			for _, dep := range p.DependsOn {
				if dep == n {
					indeg[m]--
					if indeg[m] == 0 {
						queue = append(queue, m)
					}
				}
			}
		}
	}
	if len(out) != len(want) {
		return nil, fmt.Errorf("pass dependency cycle detected among %v", requested)
	}
	return out, nil
}

// sectionUpToDate returns true when the sidecar already has a stored
// section for this pass with a matching Algo string. The runner uses
// this to skip recomputation for passes that haven't changed since
// the last run.
func sectionUpToDate(sc *SidecarV3, p *Pass) bool {
	if sc == nil {
		return false
	}
	switch p.Name {
	case "chapters":
		return sc.Metadata.Chapters != nil && sc.Metadata.Chapters.Algo == p.Algo
	case "paragraphs":
		return sc.Metadata.Paragraphs != nil && sc.Metadata.Paragraphs.Algo == p.Algo
	case "characters":
		return sc.Metadata.Characters != nil && sc.Metadata.Characters.Algo == p.Algo
	case "summaries":
		return sc.Metadata.Summaries != nil && sc.Metadata.Summaries.Algo == p.Algo
	}
	// Unknown section name = always run. Conservative.
	return false
}

// nowRFC3339 is the standard timestamp format for SectionMeta.ComputedAt.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
