package stt

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOpenAILive_WordTimestampFidelity exercises the REAL OpenAIClient against a
// real clip and asserts the word timestamps meet what the anchor aligner + the
// karaoke sync timeline actually need: dense per-word coverage, monotonic,
// in-range. It is a manual/live proof — skipped unless both env vars are set:
//
//	ABOOKIFY_STT_LIVE_DB   path to a monolith DB holding an openai credential
//	ABOOKIFY_STT_LIVE_CLIP path to a short spoken audio file
//
// The key is read from the DB inside the test and never printed or logged.
func TestOpenAILive_WordTimestampFidelity(t *testing.T) {
	dbPath := os.Getenv("ABOOKIFY_STT_LIVE_DB")
	clip := os.Getenv("ABOOKIFY_STT_LIVE_CLIP")
	if dbPath == "" || clip == "" {
		t.Skip("set ABOOKIFY_STT_LIVE_DB and ABOOKIFY_STT_LIVE_CLIP to run the live STT proof")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var fieldsJSON string
	if err := db.QueryRow(`SELECT fields FROM credentials WHERE provider='openai' ORDER BY id LIMIT 1`).Scan(&fieldsJSON); err != nil {
		t.Fatalf("read openai credential: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		t.Fatalf("parse fields: %v", err)
	}
	key := fields["api_key"]
	if key == "" {
		t.Fatal("no openai api_key in the vault")
	}

	res, err := NewOpenAIClient(key).TranscribeFile(clip)
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}

	// Flatten Segment.Words — exactly what generate.go persists as SyncTimestamp
	// and feeds to DetectChapters + the reader's word-sync map.
	var words []Word
	for _, s := range res.Segments {
		words = append(words, s.Words...)
	}
	textWords := len(strings.Fields(res.Text))
	t.Logf("duration=%.1fs textWords=%d syncWords=%d segments=%d", res.Duration, textWords, len(words), len(res.Segments))

	if len(words) == 0 {
		t.Fatal("no per-word timestamps — the aligner/karaoke would have nothing to map")
	}
	// Coverage: nearly every text word has a timing (tokenization can differ by a
	// few). Below ~0.9 means holes the aligner can't bridge.
	if ratio := float64(len(words)) / float64(textWords); ratio < 0.9 {
		t.Fatalf("word-timing coverage %.2f (%d/%d) too low", ratio, len(words), textWords)
	}
	// Monotonic non-decreasing starts + sane per-word intervals in range.
	for i, w := range words {
		if w.End < w.Start {
			t.Fatalf("word %d has end<start", i)
		}
		if w.Start < 0 || w.End > res.Duration+0.5 {
			t.Fatalf("word %d out of range: [%.2f,%.2f] dur=%.2f", i, w.Start, w.End, res.Duration)
		}
		if i > 0 && w.Start+1e-6 < words[i-1].Start {
			t.Fatalf("word %d start regresses (%.3f < %.3f)", i, w.Start, words[i-1].Start)
		}
	}
}
