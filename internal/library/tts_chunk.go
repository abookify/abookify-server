package library

import "strings"

// SplitTextForTTS breaks long text into chunks the TTS engine can handle,
// splitting on sentence boundaries near targetWords. This is the single shared
// implementation used by both the server's generation pipeline and tts-cli — the
// CLI must not carry its own copy (engine/orchestration logic lives in one place).
func SplitTextForTTS(text string, targetWords int) []string {
	words := strings.Fields(text)
	if len(words) <= targetWords {
		return []string{text}
	}

	var chunks []string
	sentences := splitSentences(text)
	var current []string
	currentWords := 0

	for _, sentence := range sentences {
		sentWords := len(strings.Fields(sentence))
		if currentWords+sentWords > targetWords && currentWords > 0 {
			chunks = append(chunks, strings.Join(current, " "))
			current = nil
			currentWords = 0
		}
		current = append(current, sentence)
		currentWords += sentWords
	}

	if len(current) > 0 {
		chunks = append(chunks, strings.Join(current, " "))
	}

	if len(chunks) == 0 {
		chunks = []string{text}
	}

	return chunks
}

// splitSentences splits text on sentence-ending punctuation (and newlines).
func splitSentences(text string) []string {
	var sentences []string
	var current strings.Builder

	for _, r := range text {
		current.WriteRune(r)
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			s := strings.TrimSpace(current.String())
			if s != "" {
				sentences = append(sentences, s)
			}
			current.Reset()
		}
	}

	if s := strings.TrimSpace(current.String()); s != "" {
		sentences = append(sentences, s)
	}

	return sentences
}
