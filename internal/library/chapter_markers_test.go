package library

import (
	"testing"
)

// Real ffprobe output format — taken from the docs + sample M4B file.
const m4bFixture = `{
	"chapters": [
		{
			"id": 0,
			"time_base": "1/1000",
			"start": 0,
			"start_time": "0.000000",
			"end": 2543000,
			"end_time": "2543.000000",
			"tags": {"title": "Introduction"}
		},
		{
			"id": 1,
			"time_base": "1/1000",
			"start": 2543000,
			"start_time": "2543.000000",
			"end": 5123500,
			"end_time": "5123.500000",
			"tags": {"title": "Chapter 1 — The Beginning"}
		},
		{
			"id": 2,
			"start_time": "5123.500000",
			"end_time": "8901.250000",
			"tags": {"title": "Chapter 2 — Rising Action"}
		}
	]
}`

func TestParseFFProbeChapters_M4B(t *testing.T) {
	chs, err := parseFFProbeChapters([]byte(m4bFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chs) != 3 {
		t.Fatalf("want 3 chapters, got %d", len(chs))
	}
	if chs[0].Title != "Introduction" || chs[0].StartSec != 0 || chs[0].EndSec != 2543.0 {
		t.Errorf("chapter 0: %+v", chs[0])
	}
	if chs[1].Title != "Chapter 1 — The Beginning" || chs[1].StartSec != 2543.0 {
		t.Errorf("chapter 1: %+v", chs[1])
	}
	if chs[2].EndSec != 8901.25 {
		t.Errorf("chapter 2 EndSec wrong: %v", chs[2].EndSec)
	}
	// Index is assigned in order.
	for i, c := range chs {
		if c.Index != i {
			t.Errorf("index[%d] = %d", i, c.Index)
		}
	}
}

func TestParseFFProbeChapters_NoChapters(t *testing.T) {
	// LibriVox / plain MP3 case — ffprobe still emits a valid response with empty array.
	chs, err := parseFFProbeChapters([]byte(`{"chapters": []}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chs) != 0 {
		t.Errorf("want empty, got %d chapters", len(chs))
	}
}

func TestParseFFProbeChapters_MissingTitle(t *testing.T) {
	// Some ID3 CHAP frames don't include a title. Synthesize one.
	fixture := `{"chapters":[{"start_time":"0","end_time":"100"},{"start_time":"100","end_time":"200"}]}`
	chs, _ := parseFFProbeChapters([]byte(fixture))
	if chs[0].Title != "Chapter 1" || chs[1].Title != "Chapter 2" {
		t.Errorf("synthesized titles wrong: %q, %q", chs[0].Title, chs[1].Title)
	}
}

func TestParseFFProbeChapters_Malformed(t *testing.T) {
	if _, err := parseFFProbeChapters([]byte(`not json`)); err == nil {
		t.Error("expected error on malformed input")
	}
}
