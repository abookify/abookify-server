package library

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Guarded harness, not a unit test: dumps EXACTLY the text the generator
// would send to the TTS engine for one chapter — the pause-aware segments and
// the ≤500-word chunks inside each — as JSON, so an out-of-process measurement
// (engine/tools/tts_sameness.py) can synthesize the same chunks against two
// engines and compare them per chunk. Run with:
//
//	TTS_CHUNK_DUMP_TEXT=<file> TTS_CHUNK_DUMP_TITLE="STAVE ONE." \
//	TTS_CHUNK_DUMP_OUT=<file.json> go test -run TestDumpTTSChunks ./internal/library
//
// TTS_CHUNK_DUMP_MAXSEGS truncates to the first N segments (the cadence
// harness's CADENCE_SAMPLE_MAXSEGS, same meaning).
func TestDumpTTSChunks(t *testing.T) {
	out := os.Getenv("TTS_CHUNK_DUMP_OUT")
	if out == "" {
		t.Skip("TTS_CHUNK_DUMP_OUT not set — harness, not a unit test")
	}
	raw, err := os.ReadFile(os.Getenv("TTS_CHUNK_DUMP_TEXT"))
	if err != nil {
		t.Fatal(err)
	}
	titlePause, paraPause := 1100, 500
	if n, _ := strconv.Atoi(os.Getenv("TTS_CHUNK_DUMP_TITLE_PAUSE_MS")); n > 0 {
		titlePause = n
	}
	if n, _ := strconv.Atoi(os.Getenv("TTS_CHUNK_DUMP_PARA_PAUSE_MS")); n > 0 {
		paraPause = n
	}
	segs := PreprocessForTTSSegmentsWithPauses(os.Getenv("TTS_CHUNK_DUMP_TITLE"), string(raw), titlePause, paraPause)
	if n, _ := strconv.Atoi(os.Getenv("TTS_CHUNK_DUMP_MAXSEGS")); n > 0 && len(segs) > n {
		segs = segs[:n]
	}
	type seg struct {
		Text         string   `json:"text"`
		PauseAfterMs int      `json:"pause_after_ms"`
		Chunks       []string `json:"chunks"`
	}
	var dump []seg
	for _, s := range segs {
		dump = append(dump, seg{Text: s.Text, PauseAfterMs: s.PauseAfterMs, Chunks: SplitTextForTTS(s.Text, 500)})
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("dumped %d segments to %s", len(dump), out)
}
