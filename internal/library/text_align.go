package library

import (
	"strings"
	"unicode"

	"github.com/pj/abookify/internal/db"
)

// AlignTimestampsToSource maps Whisper word timestamps back to the original source text.
// This bridges three text versions:
//   - Original ebook text (what the user sees)
//   - Preprocessed text (what Kokoro read)
//   - Whisper transcript (what Whisper heard back, with timestamps)
//
// The result is timestamps mapped to original text words so the karaoke
// highlights the real ebook text, not Whisper's interpretation.
func AlignTimestampsToSource(originalText string, whisperWords []db.SyncTimestamp) []db.SyncTimestamp {
	// Tokenize the original text into words, preserving spacing
	origTokens := tokenize(originalText)
	if len(origTokens) == 0 || len(whisperWords) == 0 {
		return whisperWords // fallback to Whisper's version
	}

	// Build normalized versions for matching
	origNorm := make([]string, len(origTokens))
	for i, t := range origTokens {
		origNorm[i] = normalizeWord(t.word)
	}

	whisperNorm := make([]string, len(whisperWords))
	for i, w := range whisperWords {
		whisperNorm[i] = normalizeWord(w.Word)
	}

	// Anchor alignment, NOT greedy matching. The greedy walker this replaces
	// carried a 10-word lookahead: one divergence longer than that (a spelled-
	// out number, a preprocessing artifact) derailed it permanently, and every
	// later word "interpolated" at prev+0.15s — Alice ch3 stored all 1,702
	// words ending at 294s of a 554s narration, with a COMPLETE whisper
	// transcription in hand. The anchor chain re-synchronizes after any
	// divergence, exactly why it superseded greedy matching everywhere else.
	aln := Align(origNorm, whisperNorm, 4)

	assign := make([]int, len(origTokens))
	for i := range assign {
		assign[i] = -1
	}
	anchored := 0
	for _, s := range aln.Segments {
		if s.Kind != SegAligned {
			continue
		}
		n := s.EbookEnd - s.EbookStart
		if m := s.TransEnd - s.TransStart; m < n {
			n = m
		}
		for k := 0; k < n; k++ {
			oi, wi := s.EbookStart+k, s.TransStart+k
			if oi < len(assign) && wi < len(whisperWords) {
				assign[oi] = wi
				anchored++
			}
		}
	}
	if anchored == 0 {
		return whisperWords // nothing matched at all — old degenerate fallback
	}

	// Unanchored words interpolate LINEARLY between the surrounding anchors'
	// times (or the audio edges), so a divergent stretch spreads across the
	// real time it occupies instead of piling up at its start.
	audioEnd := whisperWords[len(whisperWords)-1].End
	result := make([]db.SyncTimestamp, 0, len(origTokens))
	i := 0
	for i < len(origTokens) {
		if assign[i] >= 0 {
			w := whisperWords[assign[i]]
			result = append(result, db.SyncTimestamp{Start: w.Start, End: w.End, Word: displayWord(origTokens[i])})
			i++
			continue
		}
		// Run of unanchored originals [i, j).
		j := i
		for j < len(origTokens) && assign[j] < 0 {
			j++
		}
		t0 := 0.0
		if i > 0 && assign[i-1] >= 0 {
			t0 = whisperWords[assign[i-1]].End
		}
		t1 := audioEnd
		if j < len(origTokens) && assign[j] >= 0 {
			t1 = whisperWords[assign[j]].Start
		}
		if t1 < t0 {
			t1 = t0
		}
		step := (t1 - t0) / float64(j-i)
		for k := i; k < j; k++ {
			s := t0 + float64(k-i)*step
			result = append(result, db.SyncTimestamp{Start: s, End: s + step, Word: displayWord(origTokens[k])})
		}
		i = j
	}
	return result
}

func displayWord(t token) string {
	if t.leadingSpace {
		return " " + t.word
	}
	return t.word
}

type token struct {
	word         string
	leadingSpace bool
}

func tokenize(text string) []token {
	var tokens []token
	var current strings.Builder
	hadSpace := true

	for _, r := range text {
		if unicode.IsSpace(r) {
			if current.Len() > 0 {
				tokens = append(tokens, token{word: current.String(), leadingSpace: hadSpace})
				current.Reset()
			}
			hadSpace = true
		} else {
			current.WriteRune(r)
			// hadSpace is preserved — no action needed here (it was
			// last set when we emitted the previous token's trailing space).
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, token{word: current.String(), leadingSpace: hadSpace})
	}
	return tokens
}

func normalizeWord(w string) string {
	w = strings.TrimSpace(w)
	w = strings.ToLower(w)
	// Strip punctuation for matching
	w = strings.TrimFunc(w, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return w
}


