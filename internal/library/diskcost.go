package library

import "github.com/pj/abookify/internal/diskfree"

// TTSCostBytes estimates the disk a narration will consume from its word
// count. Measured on the showcase set: Kokoro MP3 comes to ~2.4 KB per word
// (Dracula: 160k words → 386 MB). The content-addressed store writes the
// chapter into a working dir and then promotes it, so the peak is roughly
// double, and a margin covers longer words and pauses: 8 KB/word.
func TTSCostBytes(words int64) int64 {
	if words <= 0 {
		return 0
	}
	return words * 8 << 10
}

// NarrationSpace is the pre-flight for a TTS job: room for the estimate on
// the generated dir, refused (with a person's sentence) when it would leave
// less than the reserve.
func NarrationSpace(generatedDir string, words int64) diskfree.Verdict {
	return diskfree.Check(generatedDir, TTSCostBytes(words), "narrate this book")
}
