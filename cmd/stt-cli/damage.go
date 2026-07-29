package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Damage detection, and why it is a direct scan rather than an inference from
// the transcript.
//
// Corrupt frames do not glitch — the decoder stops emitting for the REST of what
// it is decoding. stt-cli transcribes in fixed segments, so one damage point
// silently costs everything from it to the end of ITS SEGMENT, and the next
// segment starts clean.
//
// That bounds the loss per damage point, and it is what makes "look for gaps in
// the transcript" an unsound damage detector. The size of the resulting hole is
// (segment end − damage position), so it depends on where the damage happens to
// fall inside a chunk — not on how damaged the file is. Life of Pi proved it: the
// damage recurred on a fixed period, and against a 600 s segment the per-segment
// loss was (600 − period):
//
//	file      decode errors   damage period   largest hole   found by gap scan?
//	13.mp3                1           ~456 s        431.9 s   yes
//	27.mp3               10           ~589 s         54.9 s   NO
//	29.mp3                8           ~586 s         55.9 s   NO
//	26.mp3                6           ~579 s         63.1 s   NO
//
// The MOST damaged file in the book produced the SMALLEST hole, because its
// period sat closest to the segment length. Five damaged files were missed that
// way, costing ~1,854 words of narration that nothing reported as missing.
//
// So detect damage at the source: decode each file and count the errors. It runs
// at ~760x realtime (an 11-hour book scans in under a minute) against a
// transcription measured in hours, and unlike a gap heuristic it has no blind
// spot to be unlucky in.

// damageScanWorkers bounds concurrent ffmpeg processes. The scan is CPU-bound
// and short; this keeps it from competing with a transcription for the box.
const damageScanWorkers = 4

type damageReport struct {
	path      string
	errors    int
	truncated bool    // audio ends early, remainder is zero padding
	zeroAt    int64   // byte offset where the padding starts
	zeroMB    float64 // size of the padding
}

// minZeroRun is the literal-zero run that counts as truncation padding. Encoded
// audio carries frame headers even through silence, so a quarter-megabyte of
// literal 0x00 is never real audio — but the threshold stays well above any
// plausible tag/padding block so a normal file cannot trip it.
const minZeroRun = 256 << 10

// findZeroPadding returns the offset and length of the longest literal-zero run,
// or (0,0) if none reaches minZeroRun. Streams the file so a multi-GB input
// costs nothing in memory.
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

// scanFileDamage counts decode errors in one file. A file ffmpeg cannot open at
// all reports 0 — that is a different failure, and probeDuration already refuses
// unreadable files earlier in the run.
func scanFileDamage(path string) damageReport {
	// -v error keeps stderr to real decode failures. -f null decodes everything
	// and writes nothing.
	out, _ := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Header missing") || strings.Contains(line, "Invalid data") {
			n++
		}
	}
	r := damageReport{path: path, errors: n}
	if n > 0 {
		// Distinguish the two classes, because the remedies are opposite.
		// Corrupt frames are recoverable by re-encoding. Zero padding means the
		// audio was never written — re-encoding would silently TRUNCATE the file
		// to the padding point, so the file has to be re-acquired instead.
		if off, length := findZeroPadding(path); length > 0 {
			r.truncated = true
			r.zeroAt = off
			r.zeroMB = float64(length) / (1 << 20)
		}
	}
	return r
}

// scanDamage decodes every input file and reports those carrying corrupt frames.
func scanDamage(files []string) []damageReport {
	reports := make([]damageReport, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, damageScanWorkers)
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reports[i] = scanFileDamage(f)
		}(i, f)
	}
	wg.Wait()

	var damaged []damageReport
	for _, r := range reports {
		if r.errors > 0 {
			damaged = append(damaged, r)
		}
	}
	return damaged
}

// damagePreflight refuses to transcribe sources with corrupt frames, because the
// result is a transcript that is silently incomplete — and being silent, it
// propagates into alignment, chunks, embeddings and .abook exports before anyone
// notices. Repairing first is cheap; discovering it afterwards is not.
//
// Returns an error the caller should treat as fatal unless -allow-damaged.
func damagePreflight(files []string, allowDamaged bool) error {
	if len(files) == 0 {
		return nil
	}
	damaged := scanDamage(files)
	if len(damaged) == 0 {
		log.Printf("preflight: %d source file(s) decode cleanly", len(files))
		return nil
	}

	var corrupt, truncated []damageReport
	for _, r := range damaged {
		if r.truncated {
			truncated = append(truncated, r)
		} else {
			corrupt = append(corrupt, r)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d source file(s) are unusable as-is.\n", len(damaged), len(files))
	b.WriteString("  A decoder STOPS at bad data and emits nothing after it, so transcribing\n" +
		"  these now loses narration with no error reported.\n")

	if len(corrupt) > 0 {
		b.WriteString("\n  CORRUPT FRAMES — recoverable by re-encoding:\n")
		for _, r := range corrupt {
			fmt.Fprintf(&b, "      %-24s %d decode error(s)\n", filepath.Base(r.path), r.errors)
		}
		b.WriteString("    Repair (keeps a .orig backup, verifies duration survives):\n" +
			"        testing/repair-mp3.sh <file> [...]\n")
	}

	if len(truncated) > 0 {
		b.WriteString("\n  TRUNCATED — the audio was never written; RE-ACQUIRE these, do NOT repair:\n")
		for _, r := range truncated {
			fmt.Fprintf(&b, "      %-24s audio ends at %.2f MiB, then %.1f MB of zero padding\n",
				filepath.Base(r.path), float64(r.zeroAt)/(1<<20), r.zeroMB)
		}
		b.WriteString("    Re-encoding one of these TRUNCATES it to the padding point — it cannot\n" +
			"    recover audio that is not in the file. A zero run starting on an exact MiB\n" +
			"    boundary is the signature of an interrupted download or copy.\n")
	}

	if allowDamaged {
		log.Printf("WARNING: %s\n  Proceeding anyway (-allow-damaged).", b.String())
		return nil
	}
	return fmt.Errorf("%s\n  Re-run with -allow-damaged to transcribe them regardless", b.String())
}
