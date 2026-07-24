package server

import (
	"os"
	"path/filepath"
	"testing"
)

// SyncEngineDeviceHint maps the stt_compute_mode setting to the engine-device
// hint file: gpu→cuda, cpu→cpu, auto/unset→no file (engine auto-detects).
func TestSyncEngineDeviceHint(t *testing.T) {
	srv, store, dir := newTestServer(t)
	srv.DataDir = dir
	hint := filepath.Join(dir, "engine-device")

	readHint := func() (string, bool) {
		b, err := os.ReadFile(hint)
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatalf("read hint: %v", err)
		}
		return string(b), true
	}

	// gpu -> "cuda"
	if err := store.SetSetting("stt_compute_mode", "gpu"); err != nil {
		t.Fatal(err)
	}
	srv.SyncEngineDeviceHint()
	if got, ok := readHint(); !ok || got != "cuda\n" {
		t.Errorf("gpu: hint = %q, ok=%v; want \"cuda\\n\"", got, ok)
	}

	// cpu -> "cpu"
	store.SetSetting("stt_compute_mode", "cpu")
	srv.SyncEngineDeviceHint()
	if got, ok := readHint(); !ok || got != "cpu\n" {
		t.Errorf("cpu: hint = %q, ok=%v; want \"cpu\\n\"", got, ok)
	}

	// auto -> file removed (engine auto-detects)
	store.SetSetting("stt_compute_mode", "auto")
	srv.SyncEngineDeviceHint()
	if got, ok := readHint(); ok {
		t.Errorf("auto: hint should be absent, got %q", got)
	}

	// unset behaves like auto (no file)
	store.SetSetting("stt_compute_mode", "gpu")
	srv.SyncEngineDeviceHint() // writes cuda
	store.SetSetting("stt_compute_mode", "")
	srv.SyncEngineDeviceHint() // should clear it
	if _, ok := readHint(); ok {
		t.Error("empty mode: hint should be cleared")
	}
}

// With no DataDir configured (e.g. an odd deploy) the sync is a safe no-op.
func TestSyncEngineDeviceHint_NoDataDir(t *testing.T) {
	srv, store, _ := newTestServer(t)
	srv.DataDir = ""
	store.SetSetting("stt_compute_mode", "gpu")
	srv.SyncEngineDeviceHint() // must not panic
}
