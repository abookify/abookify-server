package main

import (
	"fmt"
	"log"
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
	path   string
	errors int
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
	return damageReport{path: path, errors: n}
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

	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d source file(s) contain corrupt frames:\n", len(damaged), len(files))
	for _, r := range damaged {
		fmt.Fprintf(&b, "      %-24s %d decode error(s)\n", filepath.Base(r.path), r.errors)
	}
	b.WriteString("  A decoder STOPS at a corrupt frame and emits nothing until the end of the\n" +
		"  current segment, so transcribing these now loses narration with no error.\n" +
		"  Repair first (keeps a .orig backup, verifies duration survives):\n" +
		"      testing/repair-mp3.sh <file> [...]")

	if allowDamaged {
		log.Printf("WARNING: %s\n  Proceeding anyway (-allow-damaged).", b.String())
		return nil
	}
	return fmt.Errorf("%s\n  Re-run with -allow-damaged to transcribe them regardless", b.String())
}
