package library

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Source-integrity scanning: does this audio file actually contain the audio it
// claims to?
//
// Two defects, found in PJ's library, with OPPOSITE remedies:
//
//   - CORRUPT FRAMES — the decoder stops at a bad frame and emits nothing until
//     the end of what it is decoding. Re-encoding rebuilds the framing and
//     recovers the audio. (Life of Pi: 16 files, +17,067 words recovered.)
//
//   - TRUNCATED — the audio simply stops and the rest of the file is zero
//     padding out to the expected size, the signature of an interrupted download
//     or copy. Re-encoding one of these SHORTENS it to the padding point; the
//     audio is not in the file and cannot be recovered locally. (Oryx and Crake:
//     4 files, 74 minutes gone, masters byte-identical.)
//
// This is the ground truth behind the UI's cause classification. It must not be
// inferred from transcript gaps: a gap only appears when the loss exceeds the
// reporting threshold, and damage that costs less is invisible — Life of Pi lost
// ~1,854 words across five files with no gap over 60s at all.
//
// NOTE ON DUPLICATION: cmd/stt-cli/damage.go carries the same logic rather than
// importing this package. That is deliberate and predates this file — stt-cli is
// kept decoupled from the server library tree (see the header of
// cmd/stt-cli/redo.go) so it builds as a standalone binary. The two must be kept
// in step by hand; the constants that matter (minZeroRun, the error substrings)
// are identical in both, and any change to what counts as damage belongs in
// both places.

// SourceScanWorkers bounds concurrent ffmpeg processes.
const SourceScanWorkers = 4

// minZeroRun is the literal-zero run that counts as truncation padding. Encoded
// audio carries frame headers even through silence, so a quarter-megabyte of
// literal 0x00 is never real audio, while the threshold stays far above any
// plausible tag or padding block.
const minZeroRun = 256 << 10

// SourceScan is one file's integrity result.
type SourceScan struct {
	Path         string `json:"path"`
	DecodeErrors int    `json:"decode_errors"`
	Truncated    bool   `json:"truncated"`
	ZeroAt       int64  `json:"zero_at,omitempty"`
	ZeroBytes    int64  `json:"zero_bytes,omitempty"`
}

// Damaged reports whether the file is unusable as-is.
func (s SourceScan) Damaged() bool { return s.DecodeErrors > 0 }

// Cause maps the scan onto the UI's vocabulary. Empty when the file is fine.
func (s SourceScan) Cause() string {
	switch {
	case !s.Damaged():
		return ""
	case s.Truncated:
		return "truncated_source"
	default:
		return "damaged_source"
	}
}

// findZeroPadding returns the offset and length of the longest literal-zero run,
// or (0,0) when none reaches minZeroRun. Streams the file.
func findZeroPadding(path string) (int64, int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	const block = 1 << 20
	buf := make([]byte, block)
	var pos, run, bestLen, bestStart int64
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == 0 {
				run++
				if run > bestLen {
					bestLen = run
					bestStart = pos + int64(i) - run + 1
				}
			} else {
				run = 0
			}
		}
		pos += int64(n)
		if err != nil {
			break
		}
	}
	if bestLen < minZeroRun {
		return 0, 0
	}
	return bestStart, bestLen
}

// ScanSourceFile decodes one file and classifies any damage. Runs at roughly
// 760x realtime, so an 11-hour book costs under a minute.
func ScanSourceFile(path string) SourceScan {
	out, _ := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Header missing") || strings.Contains(line, "Invalid data") {
			n++
		}
	}
	r := SourceScan{Path: path, DecodeErrors: n}
	if n > 0 {
		if off, length := findZeroPadding(path); length > 0 {
			r.Truncated = true
			r.ZeroAt = off
			r.ZeroBytes = length
		}
	}
	return r
}

// ScanSourceFiles scans concurrently, preserving input order.
func ScanSourceFiles(paths []string) []SourceScan {
	out := make([]SourceScan, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, SourceScanWorkers)
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = ScanSourceFile(p)
		}(i, p)
	}
	wg.Wait()
	return out
}
