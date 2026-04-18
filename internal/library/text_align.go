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

	// Dynamic programming alignment (similar to diff)
	// Find the best mapping of whisper words to original words
	aligned := alignWords(origNorm, whisperNorm)

	// Build result: original words with timestamps from their aligned Whisper counterparts
	var result []db.SyncTimestamp
	for _, pair := range aligned {
		if pair.origIdx >= 0 && pair.origIdx < len(origTokens) {
			word := origTokens[pair.origIdx].word
			// Preserve leading/trailing whitespace from original
			if origTokens[pair.origIdx].leadingSpace {
				word = " " + word
			}

			if pair.whisperIdx >= 0 && pair.whisperIdx < len(whisperWords) {
				// Have a timestamp from Whisper
				result = append(result, db.SyncTimestamp{
					Start: whisperWords[pair.whisperIdx].Start,
					End:   whisperWords[pair.whisperIdx].End,
					Word:  word,
				})
			} else {
				// No Whisper match — interpolate timestamp from neighbors
				start, end := interpolateTimestamp(result, pair.origIdx, len(origTokens), whisperWords)
				result = append(result, db.SyncTimestamp{
					Start: start,
					End:   end,
					Word:  word,
				})
			}
		}
	}

	if len(result) == 0 {
		return whisperWords
	}

	return result
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

type alignPair struct {
	origIdx    int
	whisperIdx int
}

// alignWords uses a greedy forward-matching approach.
// For each original word, find the nearest matching Whisper word.
func alignWords(orig, whisper []string) []alignPair {
	var pairs []alignPair
	wi := 0 // whisper index pointer

	for oi := 0; oi < len(orig); oi++ {
		if orig[oi] == "" {
			pairs = append(pairs, alignPair{origIdx: oi, whisperIdx: -1})
			continue
		}

		// Look ahead in whisper for a match (within a window)
		bestWi := -1
		window := 10
		for j := wi; j < len(whisper) && j < wi+window; j++ {
			if whisper[j] == orig[oi] {
				bestWi = j
				break
			}
			// Also try fuzzy match (first 3+ chars)
			if len(orig[oi]) >= 3 && len(whisper[j]) >= 3 &&
				orig[oi][:3] == whisper[j][:3] {
				bestWi = j
				break
			}
		}

		if bestWi >= 0 {
			pairs = append(pairs, alignPair{origIdx: oi, whisperIdx: bestWi})
			wi = bestWi + 1
		} else {
			// No match — original word has no Whisper counterpart
			pairs = append(pairs, alignPair{origIdx: oi, whisperIdx: -1})
		}
	}

	return pairs
}

// interpolateTimestamp estimates timing for words that Whisper missed.
func interpolateTimestamp(existing []db.SyncTimestamp, origIdx, totalOrig int, whisper []db.SyncTimestamp) (float64, float64) {
	// Use the previous word's end time as start
	if len(existing) > 0 {
		prev := existing[len(existing)-1]
		gap := 0.15 // estimated word duration
		return prev.End, prev.End + gap
	}

	// No previous — use start of audio
	return 0, 0.15
}
