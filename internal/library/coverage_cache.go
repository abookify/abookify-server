package library

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pj/abookify/internal/db"
)

// coverageSummary is everything BuildCoverage needs from an alignment
// payload, reduced from the multi-MB JSON once: the word counts, the
// divergence tally, the embedding match quality, and the narrated-extra
// word count folded out of Segments. A row's summary is a few dozen bytes;
// its payload is up to 30 MB (the library's 50 anchor rows total ~190 MB),
// and GET /api/works used to parse all of it on every request — 6–22 s on
// tank, past the mobile client's 20 s timeout.
type coverageSummary struct {
	EbookWords   int
	TransWords   int
	MatchQuality float64
	Divergence   DivergenceSummary
	Extra        int // narratedExtraWords(Segments)
}

func summarizePayload(p AnchorAlignmentPayload) coverageSummary {
	return coverageSummary{
		EbookWords:   p.EbookWords,
		TransWords:   p.TransWords,
		MatchQuality: p.MatchQuality,
		Divergence:   p.Divergence,
		Extra:        narratedExtraWords(p.Segments),
	}
}

// AlignmentSummaryCache memoises coverageSummary per alignments row. The key
// is the row's identity AND its rewrite counter (id, rev, length(pairs)):
// SaveAlignment bumps rev on every upsert, so a re-alignment is a miss and
// the stale entry is simply never read again. (Not updated_at: that is
// whole seconds, and the test re-aligned twice inside one with same-length
// payloads — the summary was served stale.)
// Books, chapters and sync data are NOT cached — BuildCoverage reads those
// live, they are cheap. Safe for concurrent use. A nil *AlignmentSummaryCache
// means "parse every time" (the pre-cache behaviour), so callers that run
// once per job keep working with no cache at all.
type AlignmentSummaryCache struct {
	mu    sync.Mutex
	m     map[string]coverageSummary
	loads atomic.Int64 // payload parses performed (tests + the boot log)
}

func NewAlignmentSummaryCache() *AlignmentSummaryCache {
	return &AlignmentSummaryCache{m: map[string]coverageSummary{}}
}

// Loads is how many payloads have been read and parsed since construction.
func (c *AlignmentSummaryCache) Loads() int64 {
	if c == nil {
		return 0
	}
	return c.loads.Load()
}

// summaryKey is (id, rev): the id is never reissued (AUTOINCREMENT) and rev
// moves on every rewrite of the row. Deliberately nothing derived from the
// payload itself — length(pairs) on a TEXT column reads the whole value, and
// so does any column stored after it (see rebuildAlignmentsBlobLast).
func summaryKey(a *db.Alignment) string {
	return fmt.Sprintf("%d|%d", a.ID, a.Rev)
}

// summaryFor returns the row's summary, from the cache when the row's stamp
// matches, else by reading and parsing the blob (and caching it). ok is false
// when the blob is missing or unparseable — the same rows BuildCoverage
// skipped before.
//
// a may come from ListAlignmentMetaForWork (Pairs empty) or from
// ListAlignmentsForWork (Pairs loaded): a loaded blob is parsed directly
// and still cached under its stamp.
func (c *AlignmentSummaryCache) summaryFor(store *db.Store, a *db.Alignment) (coverageSummary, bool) {
	var key string
	if c != nil {
		key = summaryKey(a)
		c.mu.Lock()
		s, hit := c.m[key]
		c.mu.Unlock()
		if hit {
			return s, true
		}
	}
	pairs := a.Pairs
	if pairs == "" {
		var err error
		if pairs, err = store.GetAlignmentPairs(a.ID); err != nil || pairs == "" {
			return coverageSummary{}, false
		}
	}
	if c != nil {
		c.loads.Add(1)
	}
	var p AnchorAlignmentPayload
	if json.Unmarshal([]byte(pairs), &p) != nil {
		return coverageSummary{}, false
	}
	s := summarizePayload(p)
	if c != nil {
		c.mu.Lock()
		c.m[key] = s
		c.mu.Unlock()
	}
	return s, true
}

// Warm parses every alignments row's payload not already cached, so the
// first GET /api/works after boot is as fast as the hundredth. Returns the
// number of rows parsed. Meant for a goroutine on the boot→ready edge: a
// request that arrives mid-warm parses what it needs itself, which is what
// it would have done anyway.
func (c *AlignmentSummaryCache) Warm(store *db.Store) int {
	if c == nil {
		return 0
	}
	rows, err := store.ListAlignmentMeta()
	if err != nil {
		return 0
	}
	n := 0
	for i := range rows {
		before := c.loads.Load()
		c.summaryFor(store, &rows[i])
		if c.loads.Load() > before {
			n++
		}
	}
	return n
}
