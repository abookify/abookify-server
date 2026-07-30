package library

import (
	"fmt"
	"log"
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

	// Whisper's own per-word confidence, which is the closest cheap proxy we have
	// for "does this text match the audio" — every other check only asks whether
	// the text looks odd on its own terms.
	//
	// Calibrated on the library, not guessed. In books with fabricated spans the
	// fabricated words carry a median confidence of 0.25-0.78 while the rest of
	// the same book sits at 0.999-1.000. A RUN of consecutive low-confidence
	// words is required rather than a windowed average, because a windowed mean
	// dilutes a short fabricated pocket with the good narration around it and
	// missed 10 of 57 damaged books that way.
	//
	// At floor 0.50 / run 8 this independently rediscovers 57 of 57 books already
	// known damaged, with no book flagged that was not, using confidence ALONE and
	// never looking at word timings. Two unrelated measurements of the same failed
	// decode converging exactly is what makes the signal trustworthy.
	lowConfFloor  = 0.50
	minLowConfRun = 8
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

	// 4. Runs of words the model itself did not believe. This is the only check
	// here that asks about the RELATIONSHIP between text and audio rather than
	// about the text in isolation, so it is the one that can catch fabricated
	// prose carrying structurally plausible timings.
	//
	// Skipped entirely when the sidecar has no confidence data (pre-v2), rather
	// than treating absent confidence as zero and flagging the whole book.
	if hasConf(sc.Words) {
		if n, at := lowConfidenceRun(sc.Words); n > 0 {
			out = append(out, SidecarProblem{
				Kind: "model_did_not_believe_this",
				Detail: fmt.Sprintf("%d word(s) sit in runs of %d+ consecutive words below %.2f "+
					"confidence (worst run starts %.1fs) — Whisper did not believe its own output "+
					"here, which in this library has meant the text does not match the audio",
					n, minLowConfRun, lowConfFloor, at),
				AtSec: at, Count: n,
			})
		}
	}

	// 5. Sources must still tile the timeline; a hole or overlap means the file
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

// CheckTranscriptIntegrity validates a fresh transcription result before its
// word timings are written to the database.
//
// The in-app Transcribe job never produces a sidecar file — it goes straight from
// ChunkedTranscribe to SaveSyncData — so neither the write-time check in stt-cli
// nor the import-time check above ever sees it. Until this existed, a
// transcription started from the UI got NO integrity validation at all, while the
// identical work run through stt-cli got two.
//
// duration is the audio's length; sources may be nil for a single-file work.
func CheckTranscriptIntegrity(words []sttWord, sources []sttSource, duration float64) []SidecarProblem {
	return checkSidecarIntegrity(&sttSidecar{
		Duration: duration,
		Words:    words,
		Sources:  sources,
	})
}

// LogTranscriptProblems reports integrity defects found in a fresh
// transcription. Loud and specific, because the failure being guarded is a run
// that LOOKS successful — fabricated text arrives with a HIGHER word count than
// the truth, so every progress figure reads better rather than worse.
func LogTranscriptProblems(label string, words []sttWord, sources []sttSource, duration float64) int {
	problems := CheckTranscriptIntegrity(words, sources, duration)
	if len(problems) == 0 {
		return 0
	}
	log.Printf("stt: INTEGRITY — %s has %d structural problem(s); its word count is NOT trustworthy:",
		label, len(problems))
	for _, p := range problems {
		log.Printf("stt: INTEGRITY   [%s] %s", p.Kind, p.Detail)
	}
	return len(problems)
}

// hasConf reports whether the sidecar carries per-word confidence at all.
// Pre-v2 sidecars do not, and absent confidence must not read as zero.
func hasConf(words []sttWord) bool {
	for _, w := range words {
		if w.Probability > 0 {
			return true
		}
	}
	return false
}

// lowConfidenceRun returns how many words sit inside runs of minLowConfRun or
// more consecutive words below lowConfFloor, and where the longest such run
// starts.
func lowConfidenceRun(words []sttWord) (int, float64) {
	var total, cur, best int
	var bestAt, curAt float64
	flush := func() {
		if cur >= minLowConfRun {
			total += cur
			if cur > best {
				best, bestAt = cur, curAt
			}
		}
		cur = 0
	}
	for _, w := range words {
		if w.Probability < lowConfFloor {
			if cur == 0 {
				curAt = w.Start
			}
			cur++
		} else {
			flush()
		}
	}
	flush()
	return total, bestAt
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
