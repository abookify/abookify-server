package library

import (
	"strconv"
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

// ttsPiece is one synthesized chunk plus the silence that follows it.
type ttsPiece struct {
	audio        []byte
	pauseAfterMs int
}

// concatAudioPieces is concatAudioChunks with real inserted silence — the
// cadence work (PJ, 2026-08-10): punctuation barely moves Kokoro's rhythm,
// so the pause after a chapter title and between paragraphs is inserted as
// actual silence at assembly time. The silence files are generated to MATCH
// the probed sample rate/channel layout of the first chunk — the concat
// demuxer requires uniform stream parameters, and a mismatched silence
// would corrupt the seam exactly the way byte-concat used to.
func concatAudioPieces(pieces []ttsPiece, outPath string) error {
	if len(pieces) == 0 {
		return fmt.Errorf("no audio pieces")
	}
	if err := concatAudioPiecesWithSilence(pieces, outPath); err != nil {
		// The pauses are an enhancement, not correctness — a missing ffmpeg
		// or a failed silence render must not fail the chapter. Fall back to
		// the flat concat (which has its own guarded fallbacks), loudly.
		log.Printf("tts: pause insertion failed (%v) — assembling without pauses", err)
		chunks := make([][]byte, len(pieces))
		for i := range pieces {
			chunks[i] = pieces[i].audio
		}
		return concatAudioChunks(chunks, outPath)
	}
	return nil
}

func concatAudioPiecesWithSilence(pieces []ttsPiece, outPath string) error {
	needSilence := false
	for _, p := range pieces[:len(pieces)-1] {
		if p.pauseAfterMs > 0 {
			needSilence = true
		}
	}
	if !needSilence {
		chunks := make([][]byte, len(pieces))
		for i := range pieces {
			chunks[i] = pieces[i].audio
		}
		return concatAudioChunks(chunks, outPath)
	}
	dir, err := os.MkdirTemp("", "tts-concat-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	first := filepath.Join(dir, "c0000.mp3")
	if err := os.WriteFile(first, pieces[0].audio, 0644); err != nil {
		return err
	}
	rate, ch := probeAudioParams(first)
	silences := map[int]string{}
	var list strings.Builder
	for i, p := range pieces {
		cp := filepath.Join(dir, fmt.Sprintf("c%04d.mp3", i))
		if i > 0 {
			if err := os.WriteFile(cp, p.audio, 0644); err != nil {
				return err
			}
		}
		fmt.Fprintf(&list, "file '%s'\n", cp)
		if p.pauseAfterMs > 0 && i < len(pieces)-1 {
			sp, ok := silences[p.pauseAfterMs]
			if !ok {
				sp = filepath.Join(dir, fmt.Sprintf("s%d.mp3", p.pauseAfterMs))
				cl := "mono"
				if ch == 2 {
					cl = "stereo"
				}
				cmd := exec.Command("ffmpeg", "-y", "-v", "error",
					"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", rate, cl),
					"-t", fmt.Sprintf("%.3f", float64(p.pauseAfterMs)/1000),
					"-c:a", "libmp3lame", "-q:a", "3", sp)
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("silence generation failed: %v: %s", err, out)
				}
				silences[p.pauseAfterMs] = sp
			}
			fmt.Fprintf(&list, "file '%s'\n", sp)
		}
	}
	listPath := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0644); err != nil {
		return err
	}
	cmd := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c:a", "libmp3lame", "-q:a", "3", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg concat with pauses failed: %v: %s", err, out)
	}
	return nil
}

// probeAudioParams returns (sample_rate, channels) of an audio file,
// defaulting to Kokoro's 24000/mono when probing fails.
func probeAudioParams(path string) (int, int) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,channels", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 24000, 1
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) != 2 {
		return 24000, 1
	}
	rate, _ := strconv.Atoi(parts[0])
	ch, _ := strconv.Atoi(parts[1])
	if rate == 0 {
		rate = 24000
	}
	if ch == 0 {
		ch = 1
	}
	return rate, ch
}
