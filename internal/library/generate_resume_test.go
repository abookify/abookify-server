package library

import "testing"

// A restart must resume the job that was ASKED FOR. The old resume path called
// GenerateAudioFromText(work, 0, "", "") — settings-default voice — which
// turned an interrupted bm_fable Carol job into an af_heart generation. The
// job id encodes the request; the parser must invert it exactly.
func TestParseTTSJobID(t *testing.T) {
	w, b, v, ok := parseTTSJobID("tts-85-111504-bm-fable")
	if !ok || w != 85 || b != 111504 || v != "bm_fable" {
		t.Errorf("got (%d,%d,%q,%v)", w, b, v, ok)
	}
	if _, _, _, ok := parseTTSJobID("regen-24-14381-5"); ok {
		t.Errorf("regen id must not parse as a tts job")
	}
	if _, _, _, ok := parseTTSJobID("tts-85"); ok {
		t.Errorf("truncated id must not parse")
	}
}
