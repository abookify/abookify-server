package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1 sidecars had no "version" field at all.
const v1Sample = `{
  "language": "en",
  "duration": 123.4,
  "text": "hello world",
  "words": [
    {"s": 0.0, "e": 0.5, "w": "Hello"},
    {"s": 0.5, "e": 1.0, "w": "world"}
  ],
  "chapters": [
    {"title": "Chapter 1", "start_sec": 0.0, "end_sec": 60.0, "word_idx": 0, "word_count": 1}
  ]
}`

// v2 added silences[] and sources[] alongside v1 fields.
const v2Sample = `{
  "version": 2,
  "language": "en",
  "duration": 123.4,
  "words": [
    {"s": 0.0, "e": 0.5, "w": "Hello", "conf": 0.99}
  ],
  "silences": [
    {"s": 1.0, "e": 4.0, "duration": 3.0, "source": "silencedetect", "kind": "chapter"}
  ],
  "sources": [
    {"file": "audio.mp3", "offset_secs": 0, "duration_secs": 123.4}
  ]
}`

func TestUpgradeFromV1(t *testing.T) {
	v3, err := UpgradeToV3([]byte(v1Sample))
	if err != nil {
		t.Fatalf("upgrade v1: %v", err)
	}
	if v3.Version != 3 {
		t.Errorf("version=%d, want 3", v3.Version)
	}
	if len(v3.Words) != 2 || v3.Words[0].Word != "Hello" {
		t.Errorf("words not preserved: %+v", v3.Words)
	}
	if v3.Metadata.Chapters == nil {
		t.Fatal("legacy chapters should land in metadata.chapters")
	}
	if v3.Metadata.Chapters.Algo != "legacy:stt-cli-pre-v3" {
		t.Errorf("algo=%q, want legacy:stt-cli-pre-v3", v3.Metadata.Chapters.Algo)
	}
	if got := v3.Metadata.Chapters.Entries[0].Source; got != "legacy:stt-cli-pre-v3" {
		t.Errorf("entry source=%q, want legacy tag", got)
	}
}

func TestUpgradeFromV2(t *testing.T) {
	v3, err := UpgradeToV3([]byte(v2Sample))
	if err != nil {
		t.Fatalf("upgrade v2: %v", err)
	}
	if v3.Version != 3 {
		t.Errorf("version=%d, want 3", v3.Version)
	}
	if len(v3.Silences) != 1 || v3.Silences[0].Kind != "chapter" {
		t.Errorf("silences not preserved: %+v", v3.Silences)
	}
	if len(v3.Sources) != 1 || v3.Sources[0].File != "audio.mp3" {
		t.Errorf("sources not preserved: %+v", v3.Sources)
	}
	// v2 sample had no chapters so metadata.chapters stays nil
	if v3.Metadata.Chapters != nil {
		t.Errorf("expected no chapters section, got %+v", v3.Metadata.Chapters)
	}
}

// ReadSidecar should auto-upgrade v1 on disk and rewrite as v3.
func TestReadSidecar_UpgradesV1OnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.stt.json")
	if err := os.WriteFile(path, []byte(v1Sample), 0o644); err != nil {
		t.Fatal(err)
	}

	v3, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v3.Version != 3 {
		t.Errorf("returned version=%d, want 3", v3.Version)
	}

	// File on disk should now be v3.
	raw, _ := os.ReadFile(path)
	var probe struct {
		Version int `json:"version"`
	}
	json.Unmarshal(raw, &probe)
	if probe.Version != 3 {
		t.Errorf("on-disk version after upgrade = %d, want 3", probe.Version)
	}
	// And rewriting should be diff-friendly (indented).
	if !strings.Contains(string(raw), "\n  \"version\"") {
		t.Errorf("expected indented output, got: %s", string(raw)[:200])
	}
}

// Re-reading a v3 file should be a no-op (no rewrite).
func TestReadSidecar_V3IsPassthrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.stt.json")

	original := &SidecarV3{
		Version:  3,
		Schema:   "abookify-sidecar/v3",
		Duration: 100,
		Words:    []SidecarWord{{Start: 0, End: 1, Word: "hi"}},
	}
	if err := WriteSidecar(path, original); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Duration != 100 || len(got.Words) != 1 {
		t.Errorf("v3 passthrough lost data: %+v", got)
	}
}

func TestReadSidecar_UnknownVersionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.stt.json")
	os.WriteFile(path, []byte(`{"version": 99}`), 0o644)
	if _, err := ReadSidecar(path); err == nil {
		t.Error("expected error for unknown version")
	}
}
