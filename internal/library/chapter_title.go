package library

import (
	"regexp"
	"strings"
	"unicode"
)

// Promoting a spoken chapter title out of the narrator's announcement.
//
// The silence/pattern detector names chapters after the number it matched —
// "Chapter 1", "Chapter 2" — while the narrator usually announces a real title
// straight after it. 438 Days is the clearest case: fourteen chapters all named
// "Chapter N", every one of them opening with the title the narrator actually
// read.
//
//	title "Chapter 1"   body "Chapter 1. The Sharkers. His name was Salvador…"
//
// So the title is thrown away while sitting in plain view two words into the body.
//
// WHAT THIS DELIBERATELY DOES NOT DO: remove the announcement from the body.
// The narrator says those words, so a transcript that omits them is wrong — and
// more concretely, the reader's karaoke depends on DOM words matching sync words
// ONE TO ONE for transcripts (see the comment in loadSyncData). Dropping words
// from the content would desynchronise highlighting for every transcript in the
// library. The announcement staying in the body is correct; only the empty title
// was the defect.
var (
	// Leading "Chapter 12." / "Part Two" / "Chapter IV:" — the announcement the
	// detector already parsed into Number.
	announceRe = regexp.MustCompile(`(?i)^\s*(chapter|part|book|section)\s+` +
		`([0-9]+|[ivxlc]+|one|two|three|four|five|six|seven|eight|nine|ten|` +
		`eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|` +
		`nineteen|twenty|thirty|forty|fifty)\b\s*[\.\:\,\-—]*\s*`)

	// A title ends at the first sentence boundary.
	sentenceEndRe = regexp.MustCompile(`[\.\!\?]`)

	// A spoken datestamp — "November 23, 2012" — which many audiobooks read out
	// immediately after the chapter title. It is never part of the title, so it
	// is a reliable place to stop when Whisper supplied no punctuation.
	// Requires a FOLLOWING number so a title that merely contains a month
	// ("The Ides of March") is not truncated at it.
	monthRe = regexp.MustCompile(`(?i)^(january|february|march|april|may|june|july|` +
		`august|september|october|november|december)$`)
)

// maxSpokenTitleWords caps how much of the opening sentence may be taken as a
// title. Real chapter titles are short; a longer run is prose.
const maxSpokenTitleWords = 6

// TitleFromAnnouncement returns a chapter title recovered from the narrator's
// spoken announcement at the start of content, or "" when nothing can be
// recovered without guessing.
//
// Conservative by design, because a wrong title is worse than a plain one:
//
//   - the phrase must be delimited by a sentence boundary, so "Chapter Two A
//     Stormy Tribe Salvador Alvarenga awoke" — no punctuation, no way to tell
//     where a title ends and the prose begins — yields nothing rather than a
//     guess;
//   - it must contain no digits, so "A Year at Sea November 3, 1911" (title run
//     together with a datestamp) is rejected;
//   - it must be short, and must not be another announcement.
func TitleFromAnnouncement(content string) string {
	m := announceRe.FindStringIndex(content)
	if m == nil {
		return ""
	}
	rest := content[m[1]:]

	// The candidate title runs to the first sentence boundary. When Whisper
	// supplied no punctuation at all, fall back to stopping at a spoken
	// datestamp, which many audiobooks read straight after the title.
	var cand string
	if end := sentenceEndRe.FindStringIndex(rest); end != nil {
		cand = strings.TrimSpace(rest[:end[0]])
	} else {
		cand = strings.TrimSpace(titleBeforeDate(rest))
	}
	if cand == "" {
		return ""
	}

	// Reject a repeated announcement ("Chapter 5. Chapter 5. Adrift.") BEFORE
	// trimming a datestamp — the trim would cut "Chapter 5" at its digit and
	// leave a bare "Chapter", which no longer looks like an announcement and
	// would slip through as a title.
	if announceRe.MatchString(cand) {
		return ""
	}

	// A datestamp can also FOLLOW the title inside the first sentence
	// ("A Year at Sea November 3, 1911"), which would otherwise be rejected
	// wholesale for containing digits. Trim it and keep the title.
	if trimmed := strings.TrimSpace(titleBeforeDate(cand)); trimmed != "" {
		cand = trimmed
	}

	words := strings.Fields(cand)
	if len(words) == 0 || len(words) > maxSpokenTitleWords {
		return ""
	}
	for _, r := range cand {
		if unicode.IsDigit(r) {
			return ""
		}
	}
	// Guard against a second announcement ("Chapter 1. Chapter 1.") and against
	// picking up a stray lowercase fragment mid-sentence.
	if announceRe.MatchString(cand) {
		return ""
	}
	if r := []rune(cand)[0]; !unicode.IsUpper(r) {
		return ""
	}
	return cand
}

// titleBeforeDate returns the leading words of s up to a spoken datestamp — a
// month name followed by a number, or a bare number. Returns s unchanged when
// no datestamp is present, and "" when one starts immediately.
//
// This is what recovers a title from an unpunctuated announcement:
// "Adrift November 23, 2012 Position, 280 miles" -> "Adrift". A month with no
// number after it is left alone, so "The Ides of March" survives intact.
func titleBeforeDate(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		bare := strings.Trim(w, ".,;:!?\u2014-")
		isDate := false
		if monthRe.MatchString(bare) {
			// Only a date when a number follows.
			if i+1 < len(words) && startsWithDigit(words[i+1]) {
				isDate = true
			}
		} else if startsWithDigit(bare) {
			isDate = true
		}
		if isDate {
			return strings.Join(words[:i], " ")
		}
	}
	return s
}

func startsWithDigit(w string) bool {
	for _, r := range w {
		return unicode.IsDigit(r)
	}
	return false
}
