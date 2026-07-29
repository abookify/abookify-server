package library

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
	_ "modernc.org/sqlite"
)

// TestCloudProviderE2E_Live proves the "add a key, run no local engine" story
// end-to-end THROUGH the resolution path: a store configured for OpenAI + a vault
// key resolves to the cloud clients (CreateSTT/TTSProvider), and those clients
// return a real result — a word-timestamped transcript and synthesized audio.
// This is what PJ hits when he switches a work to a cloud provider.
//
// Manual/live — skipped unless both env vars are set:
//
//	ABOOKIFY_CLOUD_LIVE_DB   a monolith DB holding an openai credential (read-only;
//	                         the KEY is read from it and never printed/logged)
//	ABOOKIFY_CLOUD_LIVE_CLIP a short spoken audio file to transcribe
//
// A FRESH temp store is used for resolution, so the source DB is never mutated.
func TestCloudProviderE2E_Live(t *testing.T) {
	srcDB := os.Getenv("ABOOKIFY_CLOUD_LIVE_DB")
	clip := os.Getenv("ABOOKIFY_CLOUD_LIVE_CLIP")
	if srcDB == "" || clip == "" {
		t.Skip("set ABOOKIFY_CLOUD_LIVE_DB and ABOOKIFY_CLOUD_LIVE_CLIP to run the cloud e2e")
	}

	// Read the OpenAI key read-only from the source vault (no migration, no write).
	key := readOpenAIKey(t, srcDB)
	if key == "" {
		t.Fatal("no openai api_key in the source vault")
	}

	// Fresh store configured exactly as PJ's switch would leave it.
	store, err := db.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	defer store.Close()
	if err := store.SetSetting("stt_provider", "openai"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting("tts_provider", "openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProviderCredential("openai", "", map[string]string{"api_key": key}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	// STT: resolution → cloud client → real transcript with word timestamps.
	sp := CreateSTTProvider(store, "http://localhost:5200")
	if sp == nil || sp.Name() != "openai-whisper" {
		t.Fatalf("expected cloud STT client, got %v", nameOf(sp))
	}
	res, err := sp.TranscribeFile(clip)
	if err != nil {
		t.Fatalf("cloud transcribe: %v", err)
	}
	var words int
	for _, s := range res.Segments {
		words += len(s.Words)
	}
	t.Logf("cloud STT: %q… (%d words with timestamps)", firstN(res.Text, 40), words)
	if strings.TrimSpace(res.Text) == "" || words == 0 {
		t.Fatalf("cloud STT returned no transcript/word-timestamps (text=%q words=%d)", res.Text, words)
	}

	// TTS: resolution → cloud client → real audio bytes.
	tp := CreateTTSProvider(store, "http://localhost:8880")
	if tp == nil || tp.Name() != "openai-tts" {
		t.Fatalf("expected cloud TTS client, got %v", nameOf(tp))
	}
	audio, err := tp.Synthesize("The quick brown fox jumps over the lazy dog.", "nova")
	if err != nil {
		t.Fatalf("cloud synthesize: %v", err)
	}
	t.Logf("cloud TTS: %d bytes of audio", len(audio))
	if len(audio) == 0 {
		t.Fatal("cloud TTS returned no audio")
	}
}

func readOpenAIKey(t *testing.T, dbPath string) string {
	t.Helper()
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	defer d.Close()
	var fieldsJSON string
	if err := d.QueryRow(`SELECT fields FROM credentials WHERE provider='openai' ORDER BY id LIMIT 1`).Scan(&fieldsJSON); err != nil {
		t.Fatalf("read openai credential: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		t.Fatalf("parse fields: %v", err)
	}
	return fields["api_key"]
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
