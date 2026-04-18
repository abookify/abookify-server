// Resolution of alignment paths between source materials. When two books
// don't have a direct alignment, we compose through intermediaries:
// audio → transcript (whisper-native) → ebook (edit-distance) = audio → ebook.
//
// The resolver does a BFS over the alignment graph for a work and returns
// the shortest chain of alignments connecting two books, or nil if none exists.
package library

import (
	"github.com/pj/abookify/internal/db"
)

// AlignmentStep is one hop in a resolved path. It references the alignment
// and which direction we're traversing it (from→to matches storage, or
// to→from if we're going backwards through a stored alignment).
type AlignmentStep struct {
	Alignment *db.Alignment
	Reversed  bool // true if we're reading this alignment backwards (to→from)
}

// ResolveAlignmentPath finds the shortest chain of alignments between bookA
// and bookB within the same work. Returns nil if no path exists.
// All alignments in the chain must have the same unit type.
func ResolveAlignmentPath(store *db.Store, workID, bookA, bookB int64, unit string) []AlignmentStep {
	if bookA == bookB {
		return nil // same book, no alignment needed
	}

	// Direct hit — most common case.
	direct, err := store.GetAlignment(bookA, bookB, unit)
	if err == nil && direct != nil {
		return []AlignmentStep{{
			Alignment: direct,
			Reversed:  direct.FromBookID != bookA,
		}}
	}

	// BFS through the alignment graph.
	all, err := store.ListAlignmentsForWork(workID)
	if err != nil || len(all) == 0 {
		return nil
	}

	// Filter to matching unit.
	var edges []db.Alignment
	for _, a := range all {
		if a.Unit == unit {
			edges = append(edges, a)
		}
	}
	if len(edges) == 0 {
		return nil
	}

	// BFS state.
	type bfsNode struct {
		bookID int64
		path   []AlignmentStep
	}

	visited := map[int64]bool{bookA: true}
	queue := []bfsNode{{bookID: bookA}}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		for i := range edges {
			e := &edges[i]
			var neighbor int64
			var reversed bool
			if e.FromBookID == node.bookID && !visited[e.ToBookID] {
				neighbor = e.ToBookID
				reversed = false
			} else if e.ToBookID == node.bookID && !visited[e.FromBookID] {
				neighbor = e.FromBookID
				reversed = true
			} else {
				continue
			}

			step := AlignmentStep{Alignment: e, Reversed: reversed}
			newPath := make([]AlignmentStep, len(node.path)+1)
			copy(newPath, node.path)
			newPath[len(node.path)] = step

			if neighbor == bookB {
				return newPath
			}

			visited[neighbor] = true
			queue = append(queue, bfsNode{bookID: neighbor, path: newPath})
		}
	}

	return nil // no path found
}
