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

	"github.com/pj/abookify/internal/stt"
)

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
	detectChapters := flag.Bool("detect-chapters", false, "Detect chapter boundaries in output")
	flag.Parse()

	if *audioPath == "" {
		fmt.Fprintf(os.Stderr, "Usage: stt-cli --audio <file|dir> [--whisper url] [--output result.json | --stdout] [--detect-chapters]\n")
		fmt.Fprintf(os.Stderr, "  File input → writes <audio>.stt.json next to the source\n")
		fmt.Fprintf(os.Stderr, "  Directory input → writes <dir>.stt.json next to the directory\n")
		fmt.Fprintf(os.Stderr, "  (Directories are transcribed as one logical audiobook with continuous timestamps.)\n")
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

	client := stt.NewClient(*whisperURL)
	if err := client.Health(); err != nil {
		log.Fatalf("Whisper not reachable at %s: %v", *whisperURL, err)
	}

	// Pre-probe all durations so we can show accurate overall progress / ETA.
	durations := make([]float64, len(files))
	var totalDur float64
	for i, f := range files {
		durations[i] = probeDuration(f)
		totalDur += durations[i]
	}
	if len(files) == 1 {
		log.Printf("Audio: %s (%.0fs / %.1f min)", files[0], totalDur, totalDur/60)
	} else {
		log.Printf("Audio: %d files in %s, total %.1f min", len(files), *audioPath, totalDur/60)
		for i, f := range files {
			log.Printf("  %d. %s (%.1f min)", i+1, filepath.Base(f), durations[i]/60)
		}
	}

	start := time.Now()
	var combined stt.TranscribeResult
	combined.Duration = totalDur

	var cumOffset float64
	for fi, path := range files {
		if len(files) > 1 {
			log.Printf("[%d/%d] %s (offset %.0fs)", fi+1, len(files), filepath.Base(path), cumOffset)
		}
		segResults, err := transcribeFile(client, path, durations[fi], cumOffset, start, cumOffset, totalDur)
		if err != nil {
			log.Fatalf("transcribe %s: %v", path, err)
		}
		for _, r := range segResults {
			combined.Segments = append(combined.Segments, r.Segments...)
			combined.Text += r.Text + " "
			if combined.Language == "" {
				combined.Language = r.Language
			}
		}
		cumOffset += durations[fi]
	}
	combined.Text = strings.TrimSpace(combined.Text)

	// Flatten word timestamps
	var words []wordTS
	for _, seg := range combined.Segments {
		for _, w := range seg.Words {
			words = append(words, wordTS{Word: w.Word, Start: w.Start, End: w.End, Idx: len(words)})
		}
	}

	out := struct {
		Language string        `json:"language"`
		Duration float64       `json:"duration"`
		Text     string        `json:"text"`
		Words    []wordTS      `json:"words"`
		Chapters []chapterMark `json:"chapters,omitempty"`
		Sources  []sourceInfo  `json:"sources,omitempty"`
	}{
		Language: combined.Language,
		Duration: combined.Duration,
		Text:     combined.Text,
		Words:    words,
	}

	// If we processed a directory, record each source file's offset and duration
	// so downstream tooling can map words back to their original file.
	if len(files) > 1 {
		var acc float64
		for i, f := range files {
			out.Sources = append(out.Sources, sourceInfo{
				Filename:  filepath.Base(f),
				StartSec:  acc,
				Duration:  durations[i],
			})
			acc += durations[i]
		}
	}

	if *detectChapters {
		out.Chapters = detectChapterBoundaries(words)
		log.Printf("Detected %d chapters", len(out.Chapters))
	}

	data, _ := json.MarshalIndent(out, "", "  ")

	if *output != "" {
		if err := os.WriteFile(*output, data, 0644); err != nil {
			log.Fatalf("Write output: %v", err)
		}
		log.Printf("Wrote %s (%d words, %d bytes)", *output, len(words), len(data))
	} else {
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
func transcribeFile(client *stt.Client, path string, dur, baseOffset float64, wallStart time.Time, cumDone, totalDur float64) ([]stt.TranscribeResult, error) {
	nSegs := int(dur/chunkSecs) + 1
	var results []stt.TranscribeResult

	for i := 0; i < nSegs; i++ {
		segStart := i * chunkSecs
		segPath := fmt.Sprintf("/tmp/stt-cli-seg-%04d.mp3", i)

		// Copy a chunk without re-encoding. For non-mp3 containers `-c copy`
		// still works because we read the container-level timestamps.
		cmd := exec.Command("ffmpeg", "-y", "-v", "error",
			"-ss", strconv.Itoa(segStart), "-t", strconv.Itoa(chunkSecs),
			"-i", path, "-c", "copy", segPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg split %d: %v\n%s", i, err, out)
		}

		log.Printf("  segment %d/%d (file offset %ds)...", i+1, nSegs, segStart)
		result, err := client.TranscribeFile(segPath)
		os.Remove(segPath)
		if err != nil {
			return nil, fmt.Errorf("transcribe segment %d: %w", i, err)
		}

		// Shift all timestamps into the global timeline: base offset (prior
		// files) + segment offset within this file.
		shift := baseOffset + float64(segStart)
		shifted := stt.TranscribeResult{Language: result.Language}
		for _, seg := range result.Segments {
			s := stt.Segment{Start: seg.Start + shift, End: seg.End + shift, Text: seg.Text}
			for _, w := range seg.Words {
				s.Words = append(s.Words, stt.Word{
					Word: w.Word, Start: w.Start + shift, End: w.End + shift, Probability: w.Probability,
				})
			}
			shifted.Segments = append(shifted.Segments, s)
		}
		shifted.Text = result.Text
		results = append(results, shifted)

		// ETA against total multi-file duration.
		done := cumDone + float64(segStart+chunkSecs)
		if done > totalDur {
			done = totalDur
		}
		elapsed := time.Since(wallStart)
		if done > 0 {
			rate := elapsed.Seconds() / done
			remaining := totalDur - done
			eta := time.Duration(remaining * rate * float64(time.Second))
			log.Printf("    done (%d words, %.1fx realtime, overall %.0f%%, ETA %s)",
				len(strings.Fields(result.Text)), 1/rate, 100*done/totalDur, eta.Truncate(time.Second))
		}
	}
	return results, nil
}

func probeDuration(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		log.Fatalf("ffprobe failed for %s: %v", path, err)
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

type wordTS struct {
	Word  string  `json:"w"`
	Start float64 `json:"s"`
	End   float64 `json:"e"`
	Idx   int     `json:"-"`
}

type chapterMark struct {
	Title    string  `json:"title"`
	StartSec float64 `json:"start_sec"`
	WordIdx  int     `json:"word_idx"`
}

// sourceInfo records where each original file sits on the combined timeline
// when --audio is a directory. Lets a consumer map a global timestamp back
// to "file N at t=X within that file".
type sourceInfo struct {
	Filename string  `json:"filename"`
	StartSec float64 `json:"start_sec"`
	Duration float64 `json:"duration"`
}

func detectChapterBoundaries(words []wordTS) []chapterMark {
	norm := make([]string, len(words))
	for i, w := range words {
		norm[i] = strings.ToLower(strings.TrimFunc(w.Word, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		}))
	}

	var chapters []chapterMark

	numberWords := map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
		"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
		"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20,
		"twentyone": 21, "twentytwo": 22, "twentythree": 23, "twentyfour": 24, "twentyfive": 25,
		"twentysix": 26, "twentyseven": 27, "twentyeight": 28, "twentynine": 29, "thirty": 30,
	}

	for i := 0; i < len(norm)-1; i++ {
		if norm[i] != "chapter" {
			continue
		}
		next := norm[i+1]
		var num int
		if n, ok := numberWords[next]; ok {
			num = n
		} else if n, err := strconv.Atoi(next); err == nil {
			num = n
		} else {
			continue
		}
		chapters = append(chapters, chapterMark{
			Title:    fmt.Sprintf("Chapter %d", num),
			StartSec: words[i].Start,
			WordIdx:  i,
		})
	}

	for i := 0; i < len(norm)-1; i++ {
		if norm[i] != "part" {
			continue
		}
		next := norm[i+1]
		var num int
		if n, ok := numberWords[next]; ok {
			num = n
		} else if n, err := strconv.Atoi(next); err == nil {
			num = n
		} else {
			continue
		}
		chapters = append(chapters, chapterMark{
			Title:    fmt.Sprintf("Part %d", num),
			StartSec: words[i].Start,
			WordIdx:  i,
		})
	}

	for i := 1; i < len(chapters); i++ {
		for j := i; j > 0 && chapters[j].StartSec < chapters[j-1].StartSec; j-- {
			chapters[j], chapters[j-1] = chapters[j-1], chapters[j]
		}
	}

	return chapters
}
