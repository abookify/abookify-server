package library

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// concatAudioChunks writes one chapter's audio from independently-synthesized
// TTS chunks.
//
// MP3 chunks MUST NOT be byte-concatenated. Each chunk is a complete,
// independent encoder run, and the seams leave malformed frames. Player
// decoders skip those frames and play everything — which is exactly what made
// this bug invisible — but whisper's decoder STOPS at the first seam: the
// Gulag TTS edition's chapter-005 round-tripped as 178s of a 1757s file, and
// the word mapper then compressed all 4,523 words into that span, storing
// karaoke that drifts off the audio within the first chunk. Five TTS editions
// (Gulag, Jekyll, Alice, Sleepy Hollow, Wind in the Willows) shipped that way.
//
// ffmpeg re-encodes the chunks into one clean stream instead. If ffmpeg is
// missing the old byte concat still happens — LOUDLY — and AlignChapter's
// extent guard will refuse to store a compressed word map for the result.
func concatAudioChunks(chunks [][]byte, outPath string) error {
	if len(chunks) == 0 {
		return fmt.Errorf("no audio chunks")
	}
	if len(chunks) == 1 {
		return os.WriteFile(outPath, chunks[0], 0644)
	}
	dir, err := os.MkdirTemp("", "tts-concat-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var list strings.Builder
	for i, c := range chunks {
		p := filepath.Join(dir, fmt.Sprintf("c%04d.mp3", i))
		if err := os.WriteFile(p, c, 0644); err != nil {
			return err
		}
		fmt.Fprintf(&list, "file '%s'\n", p)
	}
	listPath := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0644); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c:a", "libmp3lame", "-q:a", "3", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("tts: ffmpeg concat failed (%v: %s) — falling back to byte concat; "+
			"the sync round-trip will likely truncate at chunk seams and the "+
			"alignment extent guard will refuse the result", err, strings.TrimSpace(string(out)))
		var all []byte
		for _, c := range chunks {
			all = append(all, c...)
		}
		return os.WriteFile(outPath, all, 0644)
	}
	return nil
}
