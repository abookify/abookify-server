package library

import (
	"regexp"
	"strings"
	"unicode"
)

// PreprocessForTTS cleans and formats chapter text for natural-sounding speech synthesis.
func PreprocessForTTS(title string, content string) string {
	var result strings.Builder

	// Strip Gutenberg boilerplate
	content = stripGutenbergBoilerplate(content)
	content = restoreParagraphBreaks(content)

	// Format the chapter title with a pause
	title = strings.TrimSpace(title)
	if title != "" && !isBoilerplateTitle(title) && !contentOpensWithTitle(content, title) {
		// Normalize all-caps titles to title case for better pronunciation
		if isAllCaps(title) {
			title = toTitleCase(title)
		}
		result.WriteString(title)
		result.WriteString(".\n\n") // Period + blank line = natural pause
	}

	// Process the body text line by line
	lines := strings.Split(content, "\n")
	prevWasBlank := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			if !prevWasBlank {
				prevWasBlank = true
			}
			continue
		}

		// Skip lines that are just the title repeated
		if strings.EqualFold(line, title) || strings.EqualFold(line, strings.ToUpper(title)) {
			continue
		}

		// Skip Gutenberg-style metadata lines
		if isBoilerplateLine(line) {
			continue
		}

		// Handle all-caps lines (sub-headings, dates, locations)
		if isAllCaps(line) && len(line) < 80 {
			line = toTitleCase(line)
			// Add pause before and after headings
			if result.Len() > 0 && !prevWasBlank {
				result.WriteString("\n\n")
			}
			result.WriteString(line)
			if !strings.HasSuffix(line, ".") && !strings.HasSuffix(line, "!") && !strings.HasSuffix(line, "?") {
				result.WriteString(".")
			}
			result.WriteString("\n\n")
			prevWasBlank = true
			continue
		}

		// Ensure line breaks between paragraphs create pauses
		if prevWasBlank && result.Len() > 0 {
			// A paragraph break should be a noticeable pause
			result.WriteString("\n\n")
		} else if result.Len() > 0 {
			// Continuation within same paragraph
			result.WriteString(" ")
		}

		// Ensure the line ends with punctuation so TTS doesn't rush
		line = ensureTrailingPunctuation(line)

		result.WriteString(line)
		prevWasBlank = false
	}

	return strings.TrimSpace(result.String())
}

func stripGutenbergBoilerplate(text string) string {
	// Remove common Gutenberg header/footer patterns
	patterns := []string{
		"*** START OF THE PROJECT GUTENBERG",
		"*** START OF THIS PROJECT GUTENBERG",
		"*** END OF THE PROJECT GUTENBERG",
		"*** END OF THIS PROJECT GUTENBERG",
		"The Project Gutenberg eBook",
		"Produced by",
		"This eBook is for the use of",
	}

	lines := strings.Split(text, "\n")
	var clean []string
	skip := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		for _, p := range patterns {
			if strings.Contains(trimmed, p) {
				skip = true
				break
			}
		}

		if skip {
			// Resume after the boilerplate section (blank line after marker)
			if trimmed == "" {
				skip = false
			}
			continue
		}

		clean = append(clean, line)
	}

	return strings.Join(clean, "\n")
}

func isBoilerplateTitle(title string) bool {
	lower := strings.ToLower(title)
	boilerplate := []string{
		"project gutenberg",
		"table of contents",
		"contents",
		"the full project",
		"license",
	}
	for _, b := range boilerplate {
		if strings.Contains(lower, b) {
			return true
		}
	}
	return false
}

func isBoilerplateLine(line string) bool {
	lower := strings.ToLower(line)
	markers := []string{
		"project gutenberg",
		"www.gutenberg.org",
		"produced by",
		"transcriber's note",
		"end of the project",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func isAllCaps(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if unicode.IsLower(r) {
				return false
			}
		}
	}
	return hasLetter
}

var wordBoundary = regexp.MustCompile(`\b\w`)

// An apostrophe is a word boundary to \b, so `\b\w` capitalised the letter
// after it too: "MARLEY'S GHOST." read "Marley'S Ghost." (seen in the Carol
// Stave One heading, 2026-09-21). Only the contraction/possessive suffixes are
// folded back — "O'BRIEN" must stay "O'Brien". NOTE: this text enters the TTS
// content key, so every chapter whose heading carries one of these suffixes
// re-synthesizes at its next generate (measured at landing: see the handoff).
var apostropheSuffix = regexp.MustCompile(`(?i)['’](s|t|ll|re|ve|d|m)\b`)

func toTitleCase(s string) string {
	lower := strings.ToLower(s)
	t := wordBoundary.ReplaceAllStringFunc(lower, strings.ToUpper)
	return apostropheSuffix.ReplaceAllStringFunc(t, strings.ToLower)
}

func ensureTrailingPunctuation(line string) string {
	// Don't add punctuation — TTS handles natural pauses.
	// Just trim trailing whitespace.
	return strings.TrimRight(line, " ")
}

// contentOpensWithTitle reports whether the body text already begins with the
// chapter title's words. Gutenberg chapters usually embed their own heading,
// and prepending the title again makes the narrator open with a stutter —
// "Stave One. Stave One. Marley's Ghost. Marley was dead..." — which is the
// first thing a listener hears.
func contentOpensWithTitle(content, title string) bool {
	tw := normalizedWords(title)
	if len(tw) == 0 {
		return false
	}
	cw := normalizedWords(content)
	if len(cw) < len(tw) {
		return false
	}
	for i := range tw {
		if cw[i] != tw[i] {
			return false
		}
	}
	return true
}

func normalizedWords(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		if len(out) >= 24 {
			break
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// restoreParagraphBreaks recovers paragraph boundaries in flat hard-wrapped
// text. The Carol epub's extraction stored chapter content with 631 single
// newlines and ZERO blank lines — so the paragraph-pause logic (both the
// old "\n\n" prose pause and the new inserted silence) never fired on it,
// which is a large part of why PJ heard one unvarying rhythm. True structure
// is unrecoverable from our stored data (content_html partial, paragraphs
// table holds wrapped LINES), so: in ~70-char-wrapped text, a SHORT line
// ending in terminal punctuation is a paragraph's last line. Conservative
// threshold — a missed break costs nothing (status quo), a false break
// inserts one 500ms pause at a sentence end, which reads as emphasis, not
// error. Whitespace-only transform: the word stream is untouched.
func restoreParagraphBreaks(content string) string {
	if strings.Contains(content, "\n\n") {
		return content // real structure present — trust it
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 8 {
		return content
	}
	var b strings.Builder
	for i, ln := range lines {
		b.WriteString(ln)
		if i == len(lines)-1 {
			break
		}
		t := strings.TrimSpace(ln)
		short := len(t) > 0 && len(t) < 45
		terminal := strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") ||
			strings.HasSuffix(t, "?") || strings.HasSuffix(t, ".”") ||
			strings.HasSuffix(t, "!”") || strings.HasSuffix(t, "?”")
		if short && terminal {
			b.WriteString("\n\n")
		} else {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TTSSegment is one independently-synthesized stretch of speech with the
// silence that should FOLLOW it in the assembled chapter. PJ's cadence
// finding (2026-08-10): Kokoro reads at one unvarying rhythm — "this than
// this and then this than this" — and punctuation-based pauses ("Title.\n\n")
// barely register. The pauses that make it sound like someone reading a book
// are INSERTED as real silence at concat time, not coaxed out of the model.
// We never need to IDENTIFY titles or paragraphs at synthesis time — we
// already hold them structurally (chMeta.Title; \n\n paragraph breaks that
// PreprocessForTTS itself maintains).
type TTSSegment struct {
	Text         string
	PauseAfterMs int
}

// Pause lengths, chosen against PJ's Stave One sample; judged by ear, not by
// whether the silence got inserted. Title gets a settled beat like a human
// narrator taking a breath after announcing the chapter; paragraphs a
// shorter one.
const (
	ttsTitlePauseMs     = 1100
	ttsParagraphPauseMs = 500
)

// PreprocessForTTSSegments is PreprocessForTTS split at the pause points:
// the (optional) spoken title as its own segment, then one segment per
// paragraph. Joining the segment texts with "\n\n" reproduces
// PreprocessForTTS's output exactly — the words are identical, only the
// silence between them is new (word-sync alignment is unaffected: silence
// adds no words, and Whisper timestamps simply carry the offsets).
func PreprocessForTTSSegments(title string, content string) []TTSSegment {
	return PreprocessForTTSSegmentsWithPauses(title, content, ttsTitlePauseMs, ttsParagraphPauseMs)
}

// PreprocessForTTSSegmentsWithPauses is PreprocessForTTSSegments with the
// pause lengths supplied — the Settings UI exposes them ("Pause after a
// chapter title", "Space between paragraphs"; PJ: "things like those gaps
// could be something built into the settings web UI"), and a setting the
// assembly never read would be decoration. Non-positive values fall back to
// the PAUSED defaults.
func PreprocessForTTSSegmentsWithPauses(title string, content string, titlePauseMs, paragraphPauseMs int) []TTSSegment {
	if titlePauseMs <= 0 {
		titlePauseMs = ttsTitlePauseMs
	}
	if paragraphPauseMs <= 0 {
		paragraphPauseMs = ttsParagraphPauseMs
	}
	full := PreprocessForTTS(title, content)
	if full == "" {
		return nil
	}
	paras := strings.Split(full, "\n\n")
	var segs []TTSSegment
	spokenTitle := ""
	t := strings.TrimSpace(title)
	if t != "" && !isBoilerplateTitle(t) && !contentOpensWithTitle(content, t) {
		if isAllCaps(t) {
			t = toTitleCase(t)
		}
		spokenTitle = t + "."
	}
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pause := paragraphPauseMs
		if len(segs) == 0 && spokenTitle != "" && p == spokenTitle {
			pause = titlePauseMs
		}
		segs = append(segs, TTSSegment{Text: p, PauseAfterMs: pause})
	}
	if len(segs) > 0 {
		segs[len(segs)-1].PauseAfterMs = 0 // chapter end: the file boundary is the pause
	}
	return segs
}
