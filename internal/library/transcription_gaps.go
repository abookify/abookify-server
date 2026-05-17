// Detect spans of a sidecar where Whisper produced no words despite the
// audio not being silent — a signature of chunked-STT failures that
// retries exhausted (stt-cli logs the failure and continues, leaving a
// silent hole in the transcript).
//
// Output is intended for two consumers:
//   - work-card UI: surface "missing transcription: 2h 5m across 3
//     segments" so the user knows content is absent
//   - selective re-transcribe: feed the spans back to the STT runner
//     so it only redoes the failed portions
package library

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/pj/abookify/internal/db"
)

func logTranscriptionGaps(audioBookID int64, gaps []TranscriptionGap, totalSec float64) {
	log.Printf("transcription-gaps: book %d has %d gap(s), %.0fs total missing",
		audioBookID, len(gaps), totalSec)
	for _, g := range gaps {
		fn := g.SourceFile
		if fn == "" {
			fn = "(unknown source)"
		}
		log.Printf("  gap %.0fs-%.0fs (%.0fs, %d words found) in %s",
			g.StartSec, g.EndSec, g.DurationSec, g.WordCount, fn)
	}
}

// TranscriptionGap is one contiguous audio span that produced no
// (or near-zero) Whisper output. Times are in seconds from the start
// of the concatenated audio timeline (matches sttWord.Start).
type TranscriptionGap struct {
	StartSec    float64 `json:"start_sec"`
	EndSec      float64 `json:"end_sec"`
	DurationSec float64 `json:"duration_sec"`
	WordCount   int     `json:"word_count"`    // words actually found in this span (typically 0-few)
	SourceFile  string  `json:"source_file,omitempty"`
}

// Gap detection tunables. Buckets are 60s windows; a bucket counts as a
// gap iff it contains fewer than minWordsPerBucket words AND its
// silence coverage is below the full-quiet threshold. Below that
// threshold the audio is mostly speech that Whisper failed to
// transcribe; above it the bucket is just a long natural pause
// (chapter break, music interlude) and we leave it alone.
const (
	gapBucketSec        = 60.0
	minWordsPerBucket   = 10   // typical narration is 130-160 wpm → ~130-160 words/bucket
	maxSilenceForGapPct = 0.75 // if >75% of the bucket is silence, it's quiet on purpose
	minReportableGapSec = 30.0 // ignore individual buckets shorter than this; merge buckets first
)

// DetectTranscriptionGaps walks the sidecar in fixed-size time buckets,
// flagging buckets where Whisper output is suspiciously sparse despite
// the audio being mostly non-silent. Adjacent gap buckets are merged
// into single spans, then mapped to source files for the report.
func DetectTranscriptionGaps(sc *sttSidecar) []TranscriptionGap {
	duration := sc.Duration
	if duration <= 0 && len(sc.Words) > 0 {
		duration = sc.Words[len(sc.Words)-1].End
	}
	if duration <= gapBucketSec {
		return nil
	}

	// Bucket the words by start-time. Linear scan is fine — typical
	// sidecars have ~250k words and ~1500 buckets.
	bucketCount := int(duration/gapBucketSec) + 1
	wordsPerBucket := make([]int, bucketCount)
	for _, w := range sc.Words {
		b := int(w.Start / gapBucketSec)
		if b >= 0 && b < bucketCount {
			wordsPerBucket[b]++
		}
	}

	// Sum silence durations per bucket. A silence can straddle buckets
	// — credit each bucket only for the portion of the silence inside it.
	silencePerBucket := make([]float64, bucketCount)
	for _, sil := range sc.Silences {
		startB := int(sil.Start / gapBucketSec)
		endB := int(sil.End / gapBucketSec)
		if startB < 0 {
			startB = 0
		}
		if endB >= bucketCount {
			endB = bucketCount - 1
		}
		for b := startB; b <= endB; b++ {
			bucketStart := float64(b) * gapBucketSec
			bucketEnd := bucketStart + gapBucketSec
			overlapStart := maxF(sil.Start, bucketStart)
			overlapEnd := minF(sil.End, bucketEnd)
			if overlapEnd > overlapStart {
				silencePerBucket[b] += overlapEnd - overlapStart
			}
		}
	}

	// Walk buckets, accumulating gap spans.
	var gaps []TranscriptionGap
	type runState struct {
		startBucket int
		wordCount   int
	}
	var run *runState
	flush := func(endBucket int) {
		if run == nil {
			return
		}
		startSec := float64(run.startBucket) * gapBucketSec
		endSec := float64(endBucket+1) * gapBucketSec
		if endSec > duration {
			endSec = duration
		}
		if endSec-startSec >= minReportableGapSec {
			gaps = append(gaps, TranscriptionGap{
				StartSec:    startSec,
				EndSec:      endSec,
				DurationSec: endSec - startSec,
				WordCount:   run.wordCount,
				SourceFile:  sourceFileAt(sc, startSec),
			})
		}
		run = nil
	}
	for b := 0; b < bucketCount; b++ {
		isQuiet := silencePerBucket[b] > gapBucketSec*maxSilenceForGapPct
		isGap := !isQuiet && wordsPerBucket[b] < minWordsPerBucket
		if isGap {
			if run == nil {
				run = &runState{startBucket: b}
			}
			run.wordCount += wordsPerBucket[b]
		} else {
			flush(b - 1)
		}
	}
	flush(bucketCount - 1)
	return gaps
}

// PersistTranscriptionGaps runs detection and writes the result to the
// audio book's transcription_gaps column. Logs a summary line so the
// import logs make gaps visible even before the UI surfaces them.
func PersistTranscriptionGaps(store *db.Store, audioBookID int64, sc *sttSidecar) error {
	gaps := DetectTranscriptionGaps(sc)
	if gaps == nil {
		gaps = []TranscriptionGap{}
	}
	enc, err := json.Marshal(gaps)
	if err != nil {
		return fmt.Errorf("marshal gaps: %w", err)
	}
	if err := store.SaveTranscriptionGaps(audioBookID, string(enc)); err != nil {
		return fmt.Errorf("save gaps: %w", err)
	}
	if len(gaps) > 0 {
		var totalSec float64
		for _, g := range gaps {
			totalSec += g.DurationSec
		}
		logTranscriptionGaps(audioBookID, gaps, totalSec)
	}
	return nil
}

// sourceFileAt returns the filename of the source MP3 covering the
// given timeline second, looking up against the sidecar's sources[]
// list. Returns "" when sources are missing or the time is outside any
// known file.
func sourceFileAt(sc *sttSidecar, t float64) string {
	for _, src := range sc.Sources {
		if t >= src.StartSec && t < src.StartSec+src.Duration {
			return src.Filename
		}
	}
	return ""
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
