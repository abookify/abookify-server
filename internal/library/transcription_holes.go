package library

import "sort"

// Hole detection: contiguous stretches with NO words at all.
//
// This exists because DetectTranscriptionGaps averages over 60 s buckets and
// therefore cannot see a hole shorter than a bucket. Life of Pi lost ~1,854
// words across five files to holes of 22-55 s — every one of them left enough
// words in its bucket to clear the 10-word threshold, so the book reported
// clean while narration was missing. A false clean is worse than a false alarm:
// nothing prompts anyone to look.
//
// The two detectors are COMPLEMENTARY and neither subsumes the other:
//
//   - buckets catch SPARSE-but-not-empty stretches — a repetition loop emits ~10
//     words per 2 minutes, so a wordless test misses it entirely;
//   - holes catch SHORT-but-empty stretches, with no resolution floor, because
//     this measures the hole itself rather than a bucket average.
//
// Validated against the pre-repair Life of Pi sidecar: at a 30 s threshold this
// flags 8 runs inside the five files the bucket scan missed completely.
const (
	// holeMinSec is the shortest wordless run worth reporting. At 30 s the
	// library yields ~6 candidate books; at 20 s it yields ~22, mostly
	// non-speech interludes. 30 s keeps the signal trustworthy while still
	// catching holes half the size of a bucket.
	holeMinSec = 30.0

	// A run that is mostly real silence is a pause, not a hole.
	holeMaxSilencePct = 0.75

	// Intros, outros and trailing silence cluster at source-file boundaries.
	// A run touching one is exempt ONLY if it is short — a long run reaching a
	// file edge is exactly what a truncated file looks like (Oryx and Crake
	// loses 38 minutes that way), and exempting those would hide the worst case.
	holeEdgeSec    = 45.0
	holeEdgeMaxSec = 90.0
)

// DetectTranscriptionHoles returns wordless stretches that are not explained by
// silence or by a file boundary. Times are on the concatenated timeline, matching
// TranscriptionGap.
func DetectTranscriptionHoles(sc *sttSidecar) []TranscriptionGap {
	duration := sc.Duration
	if duration <= 0 && len(sc.Words) > 0 {
		duration = sc.Words[len(sc.Words)-1].End
	}
	if duration <= holeMinSec || len(sc.Words) == 0 {
		return nil
	}

	sil := make([][2]float64, 0, len(sc.Silences))
	for _, s := range sc.Silences {
		sil = append(sil, [2]float64{s.Start, s.End})
	}
	sort.Slice(sil, func(i, j int) bool { return sil[i][0] < sil[j][0] })

	silenceCover := func(a, b float64) float64 {
		if b <= a {
			return 1
		}
		var t float64
		for _, s := range sil {
			if s[1] <= a {
				continue
			}
			if s[0] >= b {
				break
			}
			lo, hi := a, b
			if s[0] > lo {
				lo = s[0]
			}
			if s[1] < hi {
				hi = s[1]
			}
			if hi > lo {
				t += hi - lo
			}
		}
		return t / (b - a)
	}

	type zone struct{ a, b float64 }
	var edges []zone
	for _, s := range sc.Sources {
		st, en := s.StartSec, s.StartSec+s.Duration
		edges = append(edges, zone{st, st + holeEdgeSec}, zone{en - holeEdgeSec, en})
	}
	if len(sc.Sources) == 0 {
		edges = append(edges, zone{0, holeEdgeSec}, zone{duration - holeEdgeSec, duration})
	}
	atEdge := func(a, b float64) bool {
		for _, z := range edges {
			if !(b <= z.a || a >= z.b) {
				return true
			}
		}
		return false
	}

	var out []TranscriptionGap
	prev := 0.0
	consider := func(a, b float64) {
		if b-a < holeMinSec {
			return
		}
		if silenceCover(a, b) >= holeMaxSilencePct {
			return
		}
		if atEdge(a, b) && b-a <= holeEdgeMaxSec {
			return
		}
		out = append(out, TranscriptionGap{
			StartSec:    a,
			EndSec:      b,
			DurationSec: b - a,
			WordCount:   0,
			SourceFile:  sourceFileAt(sc, a),
		})
	}
	for _, w := range sc.Words {
		consider(prev, w.Start)
		if w.End > prev {
			prev = w.End
		}
	}
	consider(prev, duration)
	return out
}

// mergeGapSpans combines the bucket and hole detectors, dropping any hole that
// already overlaps a reported gap so a single stretch is never counted twice
// (the totals feed a "N minutes missing" figure the user reads literally).
// Results stay ordered by start time.
func mergeGapSpans(gaps, holes []TranscriptionGap) []TranscriptionGap {
	out := append([]TranscriptionGap{}, gaps...)
	for _, h := range holes {
		overlaps := false
		for _, g := range gaps {
			if h.StartSec < g.EndSec && h.EndSec > g.StartSec {
				overlaps = true
				break
			}
		}
		if !overlaps {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartSec < out[j].StartSec })
	return out
}
