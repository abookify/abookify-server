package library

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func validJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 32), uint8(y * 32), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeOKRejectsTruncated(t *testing.T) {
	good := validJPEG(t)
	if !decodeOK(good) {
		t.Error("decodeOK(valid jpeg) = false, want true")
	}
	// A JPEG cut in half is the exact "half-drawn cover" case — must be rejected.
	if decodeOK(good[:len(good)/2]) {
		t.Error("decodeOK(truncated jpeg) = true, want false")
	}
	if decodeOK([]byte("not an image at all, just bytes over 1000...")) {
		t.Error("decodeOK(garbage) = true, want false")
	}
}

func TestWriteFileAtomicAndValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "work-1.jpg")
	if err := writeFileAtomic(path, validJPEG(t)); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if !ValidateCoverFile(path) {
		t.Error("ValidateCoverFile(freshly written valid jpeg) = false")
	}
	// No leftover temp files from the atomic write.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d files, want 1 (no .tmp leftovers)", len(entries))
	}
}

func TestSweepCorruptCovers(t *testing.T) {
	dir := t.TempDir()
	writeFileAtomic(filepath.Join(dir, "work-1.jpg"), validJPEG(t)) // valid → kept
	good := validJPEG(t)
	os.WriteFile(filepath.Join(dir, "work-2.jpg"), good[:len(good)/2], 0o644) // truncated → deleted
	os.WriteFile(filepath.Join(dir, "cover-3.jpg"), []byte("garbage garbage garbage"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644) // non-cover → untouched

	checked, deleted := SweepCorruptCovers(dir)
	if checked != 3 || deleted != 2 {
		t.Errorf("checked=%d deleted=%d, want 3/2", checked, deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "work-1.jpg")); err != nil {
		t.Error("valid cover was deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "work-2.jpg")); err == nil {
		t.Error("truncated cover was NOT deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("non-cover file was touched")
	}
}
