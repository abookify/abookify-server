package library

import "testing"

func TestTTSCostBytes_ScalesWithWords(t *testing.T) {
	// Dracula-sized: 160k words → about 1.3 GB peak (actual audio 386 MB, the
	// store doubles it transiently; margin on top).
	got := TTSCostBytes(160_000)
	if got < 1<<30 || got > 2<<30 {
		t.Errorf("160k words → %d bytes; want between 1 and 2 GiB", got)
	}
	if TTSCostBytes(0) != 0 {
		t.Errorf("zero words must cost nothing")
	}
}
