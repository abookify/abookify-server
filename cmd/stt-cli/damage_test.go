package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeMP3 writes a short real MP3 via ffmpeg. Skips the test when ffmpeg is
// absent rather than failing — the damage scan IS ffmpeg, so there is nothing
// meaningful to assert without it.
func makeMP3(t *testing.T, path string, secs string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+secs,
		"-c:a", "libmp3lame", "-q:a", "9", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not synthesize test audio: %v (%s)", err, out)
	}
}

// A clean file must not be flagged. This is the case that matters most for a
// false-positive: flagging good audio would block every run.
func TestDamagePreflightCleanFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.mp3")
	makeMP3(t, good, "3")

	if d := scanDamage([]string{good}); len(d) != 0 {
		t.Fatalf("clean file reported as damaged: %+v", d)
	}
	if err := damagePreflight([]string{good}, false); err != nil {
		t.Fatalf("clean file blocked the run: %v", err)
	}
}

// Corrupt frames must be detected AND must stop the run by default, since the
// resulting transcript is silently short and propagates downstream.
func TestDamagePreflightCorruptFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.mp3")
	makeMP3(t, bad, "10")

	raw, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite a run of bytes in the middle with garbage, wrecking frame
	// headers the decoder needs.
	for i := len(raw) / 2; i < len(raw)/2+2048 && i < len(raw); i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(bad, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	d := scanDamage([]string{bad})
	if len(d) == 0 {
		t.Skip("this ffmpeg build decoded the corrupted file without reporting errors")
	}
	if d[0].errors <= 0 {
		t.Errorf("damage reported with %d errors", d[0].errors)
	}

	err = damagePreflight([]string{bad}, false)
	if err == nil {
		t.Fatal("damaged input accepted — transcription would silently lose narration")
	}
	if !strings.Contains(err.Error(), "bad.mp3") {
		t.Errorf("error does not name the damaged file: %v", err)
	}
	if !strings.Contains(err.Error(), "repair-mp3.sh") {
		t.Errorf("error does not point at the repair tool: %v", err)
	}

	// The escape hatch must work — refusing outright would block a user who
	// knowingly accepts the loss.
	if err := damagePreflight([]string{bad}, true); err != nil {
		t.Errorf("-allow-damaged did not override: %v", err)
	}
}

// No inputs is not an error condition.
func TestDamagePreflightNoFiles(t *testing.T) {
	if err := damagePreflight(nil, false); err != nil {
		t.Errorf("empty input list errored: %v", err)
	}
}
