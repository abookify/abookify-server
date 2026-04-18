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

	// Format the chapter title with a pause
	title = strings.TrimSpace(title)
	if title != "" && !isBoilerplateTitle(title) {
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

func toTitleCase(s string) string {
	lower := strings.ToLower(s)
	return wordBoundary.ReplaceAllStringFunc(lower, func(m string) string {
		return strings.ToUpper(m)
	})
}

func ensureTrailingPunctuation(line string) string {
	// Don't add punctuation — TTS handles natural pauses.
	// Just trim trailing whitespace.
	return strings.TrimRight(line, " ")
}
