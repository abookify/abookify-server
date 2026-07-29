package main

import (
	"fmt"
	"log"
	"sort"
)

// Post-write integrity checks on a sidecar.
//
// Every guard added this session validates an INPUT — is the service on GPU, is
// the directory still what the sidecar recorded, is the audio decodable. None
// validated the OUTPUT, and that is where the worst failure of the day landed:
// a redo wrote three files' words 1540s late, overlapping words that were
// already there, and the run reported "+13,619 words recovered". The number
// looked like success. It took reading the file by hand to find that a fifth of
// it was the same audio transcribed twice into the wrong place.
//
// Honest scope: these would NOT have caught that specific corruption. It was a
// clean REPLACEMENT at a wrong offset — words dropped from one range and written
// to another — which leaves no ordering, range or density trace. That case is
// caught by verifyAgainstSidecar, which validates the input before the run.
// These checks cover the adjacent classes that would otherwise ship silently:
//
//   - words must not run backwards or leave the audio's duration;
//   - no window may exceed a speech-rate ceiling, catching gross duplication;
//   - the sources array must still describe a contiguous timeline.
//
// Reported as warnings rather than failing the write: the words are already
// transcribed and discarding them helps nobody. The point is that a corrupted
// sidecar announces itself at write time instead of being believed.

const (
	// Calibrated against the library, not guessed. Legitimate books peak at 365
	// wpm over a 60s window (Orwell, 1984), with a median peak of 266. 450
	// leaves headroom above the real maximum while still catching gross
	// duplication.
	//
	// LIMIT, stated because it matters: this CANNOT detect a simple doubling.
	// Two transcriptions stacked on 150 wpm narration give 300 wpm, and
	// legitimate books reach 2.31x their own median, so a doubling sits inside
	// normal variation however the threshold is expressed. Overlap of that
	// magnitude is caught by verifyAgainstSidecar at INPUT time instead.
	maxWordsPerMinute = 450.0

	// Window for the density check. A minute is long enough that ordinary
	// bursts do not trip it and short enough to localize the damage.
	densityWindowSec = 60.0

	// Timeline drift tolerated between consecutive sources.
	sourceContiguityTolSec = 1.0
)

// checkSidecarIntegrity returns human-readable problems with a freshly built
// sidecar. Empty result means it looks structurally sound.
//
// Takes the fields rather than a struct: the writer (sidecarDoc) and the redo
// path (sidecarV3) carry deliberately separate but identical definitions, and
// both write paths must be checked without coupling them to each other.
func checkSidecarIntegrity(words []wordTS, sources []sourceInfo, duration float64) []string {
	var problems []string
	if len(words) == 0 {
		return nil
	}

	dur := duration
	if dur <= 0 {
		dur = words[len(words)-1].End
	}

	// 1. Ordering and bounds.
	outOfOrder, outOfRange := 0, 0
	prev := -1.0
	for _, w := range words {
		if w.Start < prev-0.5 { // half a second of slack for overlapping word timings
			outOfOrder++
		}
		if w.Start > prev {
			prev = w.Start
		}
		if w.Start < -0.5 || w.End > dur+1.0 {
			outOfRange++
		}
	}
	if outOfOrder > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d word(s) start before the preceding word — the timeline is not monotonic, "+
				"which is what writing a re-transcribed file at the wrong offset produces",
			outOfOrder))
	}
	if outOfRange > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d word(s) fall outside the audio (0-%.0fs)", outOfRange, dur))
	}

	// 2. Density. Two transcriptions over one stretch double its word rate.
	if worst, at := peakDensity(words, densityWindowSec); worst > maxWordsPerMinute {
		problems = append(problems, fmt.Sprintf(
			"%.0f words/min around %.0fs (ceiling %.0f) — a stretch this dense is normally "+
				"the same audio transcribed twice into one place",
			worst, at, maxWordsPerMinute))
	}

	// 3. Sources must still tile the timeline.
	if len(sources) > 1 {
		acc := sources[0].StartSec
		for i, s := range sources {
			if d := s.StartSec - acc; d > sourceContiguityTolSec || d < -sourceContiguityTolSec {
				problems = append(problems, fmt.Sprintf(
					"source %d (%s) starts at %.1fs but the preceding files end at %.1fs (%+.1fs) — "+
						"the timeline has a hole or an overlap",
					i, s.Filename, s.StartSec, acc, d))
				break
			}
			acc = s.StartSec + s.Duration
		}
	}
	return problems
}

// peakDensity returns the highest words-per-minute over any window, and where.
func peakDensity(words []wordTS, window float64) (float64, float64) {
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
		if rate := float64(i-j+1) / (window / 60.0); rate > best {
			best, bestAt = rate, starts[j]
		}
	}
	return best, bestAt
}

// reportSidecarProblems logs anything structurally wrong with a sidecar about to
// be written. Loud and specific, because the failure mode being guarded against
// is a run that LOOKS successful — the corrupted redo that started this reported
// "+13,619 words recovered" and nothing else was amiss.
func reportSidecarProblems(path string, words []wordTS, sources []sourceInfo, duration float64) {
	problems := checkSidecarIntegrity(words, sources, duration)
	if len(problems) == 0 {
		return
	}
	log.Printf("INTEGRITY: %s has %d structural problem(s) — the word count below is NOT trustworthy:", path, len(problems))
	for _, p := range problems {
		log.Printf("INTEGRITY:   %s", p)
	}
}
