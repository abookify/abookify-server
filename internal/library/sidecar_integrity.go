package library

import (
	"fmt"
	"sort"
)

// Validating a sidecar at IMPORT, not only when this machine writes one.
//
// cmd/stt-cli checks what it writes (cmd/stt-cli/integrity.go). That covers
// nothing that arrives from elsewhere, and sidecars routinely do: remote-stt
// drops them from the GPU box, syncthing carries them between machines, and the
// watcher imports any .stt.json that lands in the library. Those went straight
// into the database unexamined.
//
// The failure this guards is not hypothetical — it is what consumed today. A
// sidecar carrying ~136,000 words of fabricated text read as a completely normal
// book: the words are present, so no gap detector fires; the audio is fine, so no
// damage detector fires; and the word count is HIGHER than the truth, so every
// summary figure looks better rather than worse. Nothing in the import path
// disagreed with it.
const (
	// More than this many words sharing one timestamp means the timings were
	// synthesized rather than measured, and the text arriving with them has
	// repeatedly turned out not to match the audio. Same threshold the CLI check
	// and the whisper-side retry use, so all three agree about what is wrong.
	maxWordsPerInstant = 6

	// Narration peaks at 365 wpm over a minute in this library (Orwell); 450
	// leaves headroom while still catching gross duplication.
	maxImportWordsPerMin = 450.0
	densityWindow        = 60.0
)

// SidecarProblem is one structural defect found in an imported sidecar.
type SidecarProblem struct {
	Kind   string  `json:"kind"`
	Detail string  `json:"detail"`
	AtSec  float64 `json:"at_sec,omitempty"`
	Count  int     `json:"count,omitempty"`
}

// checkSidecarIntegrity reports structural defects in a parsed sidecar. Empty
// result means it looks sound. Cheap: one pass over the words plus a sort.
func checkSidecarIntegrity(sc *sttSidecar) []SidecarProblem {
	var out []SidecarProblem
	if len(sc.Words) == 0 {
		return nil
	}

	dur := sc.Duration
	if dur <= 0 {
		dur = sc.Words[len(sc.Words)-1].End
	}

	// 1. Collapsed timings — the signal that actually catches fabricated text.
	counts := map[float64]int{}
	worst, worstN := 0.0, 0
	collapsed, affected := 0, 0
	for _, w := range sc.Words {
		counts[w.Start]++
	}
	for t, n := range counts {
		if n > maxWordsPerInstant {
			collapsed++
			affected += n
			if n > worstN {
				worstN, worst = n, t
			}
		}
	}
	if collapsed > 0 {
		out = append(out, SidecarProblem{
			Kind: "synthesized_word_timings",
			Detail: fmt.Sprintf("%d instant(s) carry more than %d words (%d words affected, "+
				"worst %d at %.1fs) — timings were synthesized, not measured, and such spans "+
				"have not matched the audio", collapsed, maxWordsPerInstant, affected, worstN, worst),
			AtSec: worst, Count: affected,
		})
	}

	// 2. Words outside the audio, or running backwards.
	var outOfRange, backwards int
	prev := -1.0
	for _, w := range sc.Words {
		if w.Start < prev-0.5 {
			backwards++
		}
		if w.Start > prev {
			prev = w.Start
		}
		if w.Start < -0.5 || w.End > dur+1.0 {
			outOfRange++
		}
	}
	if outOfRange > 0 {
		out = append(out, SidecarProblem{Kind: "words_outside_audio",
			Detail: fmt.Sprintf("%d word(s) fall outside 0-%.0fs", outOfRange, dur), Count: outOfRange})
	}
	if backwards > 0 {
		out = append(out, SidecarProblem{Kind: "non_monotonic",
			Detail: fmt.Sprintf("%d word(s) start before the preceding word — the timeline is "+
				"not monotonic, which is what writing a re-transcribed file at the wrong offset "+
				"produces", backwards), Count: backwards})
	}

	// 3. Gross duplication. Cannot catch a plain doubling (real books reach 2.31x
	// their own median), so this is a backstop for the egregious case only.
	if rate, at := peakWordRate(sc.Words, densityWindow); rate > maxImportWordsPerMin {
		out = append(out, SidecarProblem{Kind: "implausible_density",
			Detail: fmt.Sprintf("%.0f words/min around %.0fs (ceiling %.0f) — normally the same "+
				"audio transcribed twice into one place", rate, at, maxImportWordsPerMin),
			AtSec: at})
	}

	// 4. Sources must still tile the timeline; a hole or overlap means the file
	// set changed under the sidecar.
	if len(sc.Sources) > 1 {
		acc := sc.Sources[0].StartSec
		for i, s := range sc.Sources {
			if d := s.StartSec - acc; d > 1.0 || d < -1.0 {
				out = append(out, SidecarProblem{Kind: "source_discontinuity",
					Detail: fmt.Sprintf("source %d (%s) starts at %.1fs but the preceding files end "+
						"at %.1fs (%+.1fs)", i, s.Filename, s.StartSec, acc, d), AtSec: s.StartSec})
				break
			}
			acc = s.StartSec + s.Duration
		}
	}
	return out
}

// peakWordRate returns the highest words-per-minute over any window, and where.
func peakWordRate(words []sttWord, window float64) (float64, float64) {
	if len(words) == 0 {
		return 0, 0
	}
	starts := make([]float64, len(words))
	for i, w := range words {
		starts[i] = w.Start
	}
	if !sort.Float64sAreSorted(starts) {
		sort.Float64s(starts)
	}
	var best, bestAt float64
	j := 0
	for i := range starts {
		for starts[i]-starts[j] > window {
			j++
		}
		if r := float64(i-j+1) / (window / 60.0); r > best {
			best, bestAt = r, starts[j]
		}
	}
	return best, bestAt
}
