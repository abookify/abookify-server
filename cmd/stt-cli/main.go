package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pj/abookify/internal/library"
	"github.com/pj/abookify/internal/stt"
)

// chunkSecs mirrors library.ChunkedTranscribe's 10-min window; used here only
// for the CLI's cross-file ETA estimate (the actual chunking lives in library).
const chunkSecs = 600

// audioExts are the file extensions we'll treat as audio when --audio points
// at a directory. Case-insensitive match.
var audioExts = map[string]bool{
	".mp3": true, ".m4a": true, ".m4b": true, ".flac": true,
	".wav": true, ".ogg": true, ".opus": true,
}

func main() {
	audioPath := flag.String("audio", "", "Path to audio file OR directory of audio files (processed in sorted order as one logical audiobook)")
	whisperURL := flag.String("whisper", "http://localhost:5200", "Whisper STT service URL")
	output := flag.String("output", "", "Output JSON file (default: <audio>.stt.json next to the input)")
	stdoutFlag := flag.Bool("stdout", false, "Write JSON to stdout instead of a sidecar file")
	redoFiles := flag.String("redo-files", "", "Comma-separated base filenames inside --audio dir to re-transcribe. Reads the existing sidecar, retranscribes only the named files, merges new words+silences over the old. Use to fill transcription gaps without redoing the whole book.")
	allowCPU := flag.Bool("allow-cpu", false, "proceed even if the STT service is on CPU while this host has a GPU")
	allowDamaged := flag.Bool("allow-damaged", false, "proceed even if source files contain corrupt frames (transcription will silently lose narration)")
	bootstrapSidecar := flag.Bool("bootstrap-sidecar", false, "Write a stub sidecar (sources + durations only, no words) and exit. --audio must point to a directory. The stub can then be filled chapter-by-chapter via --redo-files across multiple sessions.")
	flag.Parse()

	if *audioPath == "" {
		fmt.Fprintf(os.Stderr, "Usage: stt-cli --audio <file|dir> [--whisper url] [--output result.json | --stdout]\n")
		fmt.Fprintf(os.Stderr, "  File input → writes <audio>.stt.json next to the source\n")
		fmt.Fprintf(os.Stderr, "  Directory input → writes <dir>.stt.json next to the directory\n")
		fmt.Fprintf(os.Stderr, "  (Directories are transcribed as one logical audiobook with continuous timestamps.)\n")
		fmt.Fprintf(os.Stderr, "  Sidecar is written in v3 format: pure transcription (words + silences).\n")
		fmt.Fprintf(os.Stderr, "  Chapter detection, summaries, etc. happen server-side as post-processing passes.\n")
		os.Exit(1)
	}

	// Default sidecar path: <audio>.stt.json next to the input.
	// For a directory, strip any trailing slash first so the sidecar lands
	// beside the directory, not inside it.
	if *output == "" && !*stdoutFlag {
		base := strings.TrimRight(*audioPath, "/")
		if info, err := os.Stat(base); err == nil && !info.IsDir() {
			// File: drop the original extension, append .stt.json
			base = strings.TrimSuffix(base, filepath.Ext(base))
		}
		*output = base + ".stt.json"
	}

	files, err := resolveInputFiles(*audioPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if len(files) == 0 {
		log.Fatalf("No audio files found in %s", *audioPath)
	}

	// Pre-probe all durations so we can show accurate overall progress / ETA.
	// A file that will not probe — zero-byte or truncated download — is SKIPPED
	// with a warning rather than killing the run. A 16-hour book should not be
	// unreachable because one file in the set is a husk; this has bitten before
	// (a 170-byte truncated Sherlock file), and For Whom the Bell Tolls ships
	// with 13 zero-byte placeholders alongside its 17 real files.
	var kept []string
	var durations []float64
	var totalDur float64
	var skipped []string
	for _, f := range files {
		d, err := probeDuration(f)
		if err != nil || d <= 0 {
			skipped = append(skipped, filepath.Base(f))
			continue
		}
		kept = append(kept, f)
		durations = append(durations, d)
		totalDur += d
	}
	if len(skipped) > 0 {
		log.Printf("WARNING: skipping %d unreadable/zero-length file(s): %s",
			len(skipped), strings.Join(skipped, ", "))
	}
	files = kept
	if len(files) == 0 {
		log.Fatalf("No readable audio files in %s (%d skipped as unreadable)", *audioPath, len(skipped))
	}
	if len(files) == 1 {
		log.Printf("Audio: %s (%.0fs / %.1f min)", files[0], totalDur, totalDur/60)
	} else {
		log.Printf("Audio: %d files in %s, total %.1f min", len(files), *audioPath, totalDur/60)
		for i, f := range files {
			log.Printf("  %d. %s (%.1f min)", i+1, filepath.Base(f), durations[i]/60)
		}
	}

	// Bootstrap mode: write a stub sidecar (sources + duration, no words)
	// and exit. Subsequent --redo-files runs fill it chapter by chapter.
	// Only meaningful for directory input — single-file mode has nothing to
	// chunk.
	if *bootstrapSidecar {
		if len(files) < 2 {
			log.Fatalf("--bootstrap-sidecar requires --audio to be a directory of 2+ files (got %d)", len(files))
		}
		if *output == "" {
			log.Fatalf("--bootstrap-sidecar needs a sidecar path (--output or directory default)")
		}
		if _, err := os.Stat(*output); err == nil {
			log.Fatalf("--bootstrap-sidecar refuses to overwrite existing sidecar at %s", *output)
		}
		if err := writeBootstrapSidecar(*output, files, durations, totalDur); err != nil {
			log.Fatalf("bootstrap: %v", err)
		}
		log.Printf("Wrote stub sidecar %s (%d sources, %.1f min total, no words yet)",
			*output, len(files), totalDur/60)
		log.Printf("Fill it chapter by chapter with: stt-cli --audio %s --redo-files <filename>", *audioPath)
		return
	}

	client := stt.NewClient(*whisperURL)
	if err := client.Health(); err != nil {
		log.Fatalf("Whisper not reachable at %s: %v", *whisperURL, err)
	}

	// Corrupt source frames silently truncate a transcript, so check the audio
	// itself before spending hours on it. Deliberately ahead of the --redo-files
	// branch: a redo is usually the response to missing words, which is exactly
	// when the input is most likely to be damaged.
	if err := damagePreflight(files, *allowDamaged); err != nil {
		log.Fatalf("preflight: %v", err)
	}

	// Selective retry: only retranscribe the files named in --redo-files,
	// merging their fresh words+silences over the existing sidecar's
	// entries for those file ranges.
	if *redoFiles != "" {
		if *output == "" {
			log.Fatalf("--redo-files requires --output (or default sidecar path) — must point to an existing sidecar to merge into")
		}
		redoStart := time.Now()
		if err := retranscribeAndMerge(client, files, durations, *output, *redoFiles); err != nil {
			log.Fatalf("redo: %v", err)
		}
		log.Printf("Total: redo run finished in %s", time.Since(redoStart).Truncate(time.Second))
		return
	}

	// Verify what we are actually about to run on BEFORE committing hours.
	if err := preflight(client, *allowCPU, totalDur); err != nil {
		log.Fatalf("preflight: %v", err)
	}

	start := time.Now()
	var combined stt.TranscribeResult
	combined.Duration = totalDur

	var allSilences []silenceEvent
	var cumOffset float64
	for fi, path := range files {
		if len(files) > 1 {
			log.Printf("[%d/%d] %s (offset %.0fs)", fi+1, len(files), filepath.Base(path), cumOffset)
		}
		r, err := transcribeFile(client, path, cumOffset, start, cumOffset, totalDur)
		if err != nil {
			log.Fatalf("transcribe %s: %v", path, err)
		}
		combined.Segments = append(combined.Segments, r.Segments...)
		combined.Text += r.Text + " "
		if combined.Language == "" {
			combined.Language = r.Language
		}

		// Silences per file, here rather than in a second pass, so every
		// checkpoint below is self-consistent (words AND silences for exactly
		// the files completed so far).
		if sil, err := detectSilences(path, -30, 0.15, cumOffset); err != nil {
			log.Printf("  warning: silencedetect failed for %s: %v (continuing without)", filepath.Base(path), err)
		} else {
			log.Printf("  %s: %d silences detected", filepath.Base(path), len(sil))
			allSilences = append(allSilences, sil...)
		}
		cumOffset += durations[fi]

		// CHECKPOINT. The sidecar used to be written only after the LAST file,
		// so anything that killed a long run — a reboot, an OOM, a stopped
		// container — threw away every completed hour. Blueprint for Armageddon
		// is 23 h of audio; losing it at 90% would cost ~2 h of GPU.
		// Writing after each file caps the loss at the file in progress, and the
		// partial sidecar is directly resumable with --redo-files.
		if *output != "" && len(files) > 1 && fi < len(files)-1 {
			if err := writeSidecar(*output, &combined, allSilences, files[:fi+1], durations[:fi+1], totalDur); err != nil {
				log.Printf("  warning: checkpoint write failed: %v (continuing)", err)
			} else {
				log.Printf("  checkpoint: %d/%d files saved to %s", fi+1, len(files), filepath.Base(*output))
			}
		}
	}
	combined.Text = strings.TrimSpace(combined.Text)

	// Flatten word timestamps
	var words []wordTS
	for _, seg := range combined.Segments {
		for _, w := range seg.Words {
			words = append(words, wordTS{
				Word: w.Word, Start: w.Start, End: w.End,
				Probability: w.Probability, Idx: len(words),
			})
		}
	}

	classifySilences(allSilences)

	// Build v2 event stream: words + silences interleaved by time.
	// (event-stream merging retired in v3 — server derives what it needs from words+silences)

	// Summary
	chapterCount, paraCount, sentCount, breathCount := 0, 0, 0, 0
	for _, s := range allSilences {
		switch s.Kind {
		case "chapter":
			chapterCount++
		case "paragraph":
			paraCount++
		case "sentence":
			sentCount++
		case "breath":
			breathCount++
		}
	}
	log.Printf("Silence events: %d total (%d chapter, %d paragraph, %d sentence, %d breath)",
		len(allSilences), chapterCount, paraCount, sentCount, breathCount)

	if *output != "" {
		if err := writeSidecar(*output, &combined, allSilences, files, durations, totalDur); err != nil {
			log.Fatalf("Write output: %v", err)
		}
		fi, _ := os.Stat(*output)
		sz := int64(0)
		if fi != nil {
			sz = fi.Size()
		}
		log.Printf("Wrote %s (%d words, %d bytes)", *output, len(words), sz)
	} else {
		data, _ := json.MarshalIndent(buildSidecar(&combined, allSilences, files, durations, totalDur), "", "  ")
		os.Stdout.Write(data)
	}

	log.Printf("Total: %.1f min processed in %s", totalDur/60, time.Since(start).Truncate(time.Second))
}

// resolveInputFiles accepts either a single file or a directory. For a
// directory, it returns all audio files inside (non-recursive) in sorted order.
func resolveInputFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// transcribeFile chunks one file into ≤10-minute segments, transcribes each,
// and returns the results with timestamps shifted by `baseOffset` so the
// caller can stitch multiple files into a single timeline.
//
// `wallStart`, `cumDone`, and `totalDur` are used only for ETA logging across
// an entire multi-file run.
// transcribeFile is a thin wrapper over the shared library.ChunkedTranscribe
// primitive (chunking + retry + offset-stitch live there, in one place shared
// with the server). This wrapper only adds the CLI's cross-file ETA logging.
// Returns one combined result already shifted into the global timeline.
func transcribeFile(client *stt.Client, path string, baseOffset float64, wallStart time.Time, cumDone, totalDur float64) (*stt.TranscribeResult, error) {
	return library.ChunkedTranscribe(client, path, baseOffset, func(e library.SegmentEvent) {
		if !e.Done {
			log.Printf("  segment %d/%d (file offset %ds)...", e.SegIdx+1, e.TotalSegs, e.SegStartSecs)
			return
		}
		// ETA against total multi-file duration.
		done := cumDone + float64(e.SegStartSecs+chunkSecs)
		if done > totalDur {
			done = totalDur
		}
		elapsed := time.Since(wallStart)
		if done > 0 {
			rate := elapsed.Seconds() / done
			remaining := totalDur - done
			eta := time.Duration(remaining * rate * float64(time.Second))
			log.Printf("    done (%d words, %.1fx realtime, overall %.0f%%, ETA %s)",
				e.Words, 1/rate, 100*done/totalDur, eta.Truncate(time.Second))
		}
	})
}

// probeDuration returns the audio duration in seconds. An error (or a
// non-positive duration) means the file is unusable — the caller skips it
// rather than aborting, so one bad file in a set does not cost the whole book.
func probeDuration(path string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration for %s: %w", path, err)
	}
	return d, nil
}

type wordTS struct {
	Word        string  `json:"w"`
	Start       float64 `json:"s"`
	End         float64 `json:"e"`
	Probability float64 `json:"conf,omitempty"` // Whisper per-word confidence
	Idx         int     `json:"-"`
}

// sourceInfo records where each original file sits on the combined timeline
// when --audio is a directory. Lets a consumer map a global timestamp back
// to "file N at t=X within that file".
type sourceInfo struct {
	Filename string  `json:"filename"`
	StartSec float64 `json:"start_sec"`
	Duration float64 `json:"duration"`
}

// writeBootstrapSidecar writes a stub v3 sidecar with the sources list
// and total duration populated, but no words/silences. The result is a
// schema-valid sidecar that --redo-files can subsequently merge per-file
// transcriptions into. The watcher will not import it as-is (sidecar
// import requires non-empty words), so the database state is unchanged
// until a redo run populates it and the user explicitly drops a .redo
// marker.
func writeBootstrapSidecar(outputPath string, files []string, durations []float64, totalDur float64) error {
	var sources []sourceInfo
	var acc float64
	for i, f := range files {
		sources = append(sources, sourceInfo{
			Filename: filepath.Base(f),
			StartSec: acc,
			Duration: durations[i],
		})
		acc += durations[i]
	}

	stub := struct {
		Version  int            `json:"version"`
		Schema   string         `json:"schema"`
		Duration float64        `json:"duration"`
		Sources  []sourceInfo   `json:"sources,omitempty"`
		Words    []wordTS       `json:"words"`
		Silences []silenceEvent `json:"silences,omitempty"`
		Metadata struct{}       `json:"metadata"`
	}{
		Version:  3,
		Schema:   "abookify-sidecar/v3",
		Duration: totalDur,
		Sources:  sources,
		Words:    []wordTS{},
	}

	data, err := json.MarshalIndent(stub, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(outputPath, data, 0o644)
}

// Narrator-pattern chapter detection moved to the server's post-processing
// pipeline as of v3 sidecar (internal/library/chapter_detect.go). stt-cli
// now writes a pure-transcription sidecar with no derived metadata.

// sidecarDoc is the v3 sidecar: pure transcription. Atomic outputs only — no
// chapter detection, no event merging. The server's post-processing passes
// derive everything else from words+silences on import.
type sidecarDoc struct {
	Version  int            `json:"version"`
	Schema   string         `json:"schema"`
	Language string         `json:"language,omitempty"`
	Duration float64        `json:"duration"`
	Sources  []sourceInfo   `json:"sources,omitempty"`
	Words    []wordTS       `json:"words"`
	Silences []silenceEvent `json:"silences,omitempty"`
	Metadata struct{}       `json:"metadata"`
}

// buildSidecar assembles the document for the files completed SO FAR, which is
// what makes a mid-run checkpoint valid: `sources` describes exactly the files
// whose words are present, so --redo-files can fill the remainder.
//
// duration stays the FULL run's duration, not the partial sum — a resumed
// sidecar still describes the whole book, and the importer's file-offset
// mapping depends on it.
func buildSidecar(combined *stt.TranscribeResult, silences []silenceEvent,
	files []string, durations []float64, totalDur float64) sidecarDoc {

	var words []wordTS
	for _, seg := range combined.Segments {
		for _, w := range seg.Words {
			words = append(words, wordTS{
				Word: w.Word, Start: w.Start, End: w.End,
				Probability: w.Probability, Idx: len(words),
			})
		}
	}

	doc := sidecarDoc{
		Version:  3,
		Schema:   "abookify-sidecar/v3",
		Language: combined.Language,
		Duration: totalDur,
		Words:    words,
		Silences: silences,
	}
	if len(files) > 1 {
		var acc float64
		for i, f := range files {
			doc.Sources = append(doc.Sources, sourceInfo{
				Filename: filepath.Base(f),
				StartSec: acc,
				Duration: durations[i],
			})
			acc += durations[i]
		}
	}
	return doc
}

// writeSidecar writes the sidecar ATOMICALLY — temp file in the same directory,
// then rename. A checkpoint that lands mid-write during a crash would otherwise
// leave a truncated JSON file, which is worse than no checkpoint: it destroys
// the completed work it was meant to protect.
func writeSidecar(path string, combined *stt.TranscribeResult, silences []silenceEvent,
	files []string, durations []float64, totalDur float64) error {

	built := buildSidecar(combined, silences, files, durations, totalDur)
	// Validate the OUTPUT, not just the inputs. A corrupted sidecar otherwise
	// reads as a successful run and its word count as a recovery figure.
	reportSidecarProblems(path, built.Words, built.Sources, built.Duration)
	data, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stt-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // durable before the rename
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
