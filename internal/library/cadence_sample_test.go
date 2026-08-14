package library

import (
	"os"
	"strconv"
	"testing"

	"github.com/pj/abookify/internal/tts"
)

// Guarded harness, not a unit test: synthesizes a cadence A/B pair through
// the REAL segment + silence-insertion path (and the old flat path for
// comparison) against a live Kokoro. Run with:
//
//	CADENCE_SAMPLE_TEXT=<file> CADENCE_SAMPLE_TITLE="STAVE ONE." \
//	CADENCE_SAMPLE_OUT=<dir> go test -run TestGenerateCadenceSample
//
// The judgment is PJ's ear against the pair — same words, only the silence
// differs.
func TestGenerateCadenceSample(t *testing.T) {
	outDir := os.Getenv("CADENCE_SAMPLE_OUT")
	if outDir == "" {
		t.Skip("CADENCE_SAMPLE_OUT not set — harness, not a unit test")
	}
	raw, err := os.ReadFile(os.Getenv("CADENCE_SAMPLE_TEXT"))
	if err != nil {
		t.Fatal(err)
	}
	title := os.Getenv("CADENCE_SAMPLE_TITLE")
	voice := os.Getenv("CADENCE_SAMPLE_VOICE")
	if voice == "" {
		voice = "bm_fable"
	}
	url := os.Getenv("KOKORO_URL")
	if url == "" {
		url = "http://localhost:8880"
	}
	client := tts.NewClient(url)

	segs := PreprocessForTTSSegments(title, string(raw))
	if len(segs) < 3 {
		t.Fatalf("sample text too small: %d segments", len(segs))
	}
	if n, _ := strconv.Atoi(os.Getenv("CADENCE_SAMPLE_MAXSEGS")); n > 0 && len(segs) > n {
		segs = segs[:n]
	}
	var pieces []ttsPiece
	var flat [][]byte
	for _, seg := range segs {
		for ci, chunk := range SplitTextForTTS(seg.Text, 500) {
			audio, err := client.Synthesize(chunk, voice)
			if err != nil {
				t.Fatal(err)
			}
			pause := 0
			if ci == 0 { // segments here are single-chunk; keep it simple
				pause = seg.PauseAfterMs
			}
			pieces = append(pieces, ttsPiece{audio: audio, pauseAfterMs: pause})
			flat = append(flat, audio)
		}
	}
	// CADENCE_SAMPLE_NAME distinguishes the candidate from the approved
	// artifact BY CONSTRUCTION (an experiment must never share a path with
	// an approved file — learned by overwriting one, 2026-08-14).
	name := os.Getenv("CADENCE_SAMPLE_NAME")
	if name == "" {
		name = "stave-one-cadence"
	}
	if err := concatAudioPieces(pieces, outDir+"/"+name+"-PAUSED.mp3"); err != nil {
		t.Fatal(err)
	}
	if err := concatAudioChunks(flat, outDir+"/"+name+"-FLAT.mp3"); err != nil {
		t.Fatal(err)
	}
}
