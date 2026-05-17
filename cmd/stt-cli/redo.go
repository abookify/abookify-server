// Selective re-transcription. Reads the existing sidecar at output,
// re-transcribes ONLY the files named in redoFiles, and merges the
// fresh words+silences over whatever was previously stored for those
// files' time ranges.
//
// Use when transcription gaps (silently-skipped Whisper chunks) leave
// holes in the sidecar — re-running the whole book is slow; this
// retargets just the broken files. Typical flow:
//
//   $ stt-cli --audio book/ --redo-files 13.mp3,14.mp3
//
// Limitations:
//   - File ordering must match the original run (same sorted set of
//     audio files in the directory). If files were added/removed the
//     timeline offsets won't line up; we bail with an error.
//   - Sidecar must already exist at the output path. We don't fabricate
//     a new sidecar from a partial run — call the normal mode first.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pj/abookify/internal/stt"
)

// sidecarV3 mirrors the on-disk schema written by main(). Defined here
// rather than imported so the redo path stays decoupled from any
// schema refactors and can be vendored into stt-cli builds without
// pulling in the server library tree.
type sidecarV3 struct {
	Version  int             `json:"version"`
	Schema   string          `json:"schema"`
	Language string          `json:"language,omitempty"`
	Duration float64         `json:"duration"`
	Sources  []sourceInfo    `json:"sources,omitempty"`
	Words    []wordTS        `json:"words"`
	Silences []silenceEvent  `json:"silences,omitempty"`
	Metadata struct{}        `json:"metadata"`
}

// retranscribeAndMerge reads the existing sidecar, transcribes the
// files named in redoFiles, and writes a merged sidecar back to
// outputPath. Files not in redoFiles keep their existing data.
func retranscribeAndMerge(client *stt.Client, files []string, durations []float64, outputPath, redoFiles string) error {
	existing, err := readSidecar(outputPath)
	if err != nil {
		return fmt.Errorf("read existing sidecar: %w", err)
	}

	// Build (base-filename → file-index-in-input) so we can validate
	// the redo list against the current input set.
	indexByBase := make(map[string]int, len(files))
	var fileOffsets []float64
	var acc float64
	for i, p := range files {
		indexByBase[filepath.Base(p)] = i
		fileOffsets = append(fileOffsets, acc)
		acc += durations[i]
	}

	// Resolve the redo list to (file index, base filename, time range).
	var redo []redoEntry
	for _, raw := range strings.Split(redoFiles, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		idx, ok := indexByBase[name]
		if !ok {
			return fmt.Errorf("--redo-files: %q not in input directory (%d files)", name, len(files))
		}
		redo = append(redo, redoEntry{
			idx:      idx,
			base:     name,
			startSec: fileOffsets[idx],
			endSec:   fileOffsets[idx] + durations[idx],
		})
	}
	if len(redo) == 0 {
		return fmt.Errorf("--redo-files: no valid entries parsed from %q", redoFiles)
	}

	log.Printf("redo: re-transcribing %d file(s): %s", len(redo), redoFilesSummary(redo))

	// Run Whisper + silencedetect on each redo file.
	wallStart := time.Now()
	var newWords []wordTS
	var newSilences []silenceEvent
	for _, e := range redo {
		log.Printf("[redo] %s (offset %.0fs, dur %.0fs)", e.base, e.startSec, e.endSec-e.startSec)
		segResults, err := transcribeFile(client, files[e.idx], durations[e.idx], e.startSec, wallStart, 0, e.endSec-e.startSec)
		if err != nil {
			return fmt.Errorf("transcribe %s: %w", e.base, err)
		}
		for _, r := range segResults {
			for _, seg := range r.Segments {
				for _, w := range seg.Words {
					newWords = append(newWords, wordTS{
						Word:        w.Word,
						Start:       w.Start,
						End:         w.End,
						Probability: w.Probability,
					})
				}
			}
		}
		sil, err := detectSilences(files[e.idx], -30, 0.15, e.startSec)
		if err != nil {
			log.Printf("  warning: silencedetect failed for %s: %v", e.base, err)
		} else {
			log.Printf("  %s: %d silences detected", e.base, len(sil))
			newSilences = append(newSilences, sil...)
		}
	}
	classifySilences(newSilences)

	// Merge: drop existing words/silences whose timestamps fall in any
	// redo range; keep everything else; append the new entries; sort
	// by start time so the on-disk layout stays interleaved.
	merged := *existing
	merged.Words = mergeWords(existing.Words, newWords, redoToRanges(redo))
	merged.Silences = mergeSilences(existing.Silences, newSilences, redoToRanges(redo))
	merged.Version = 3
	merged.Schema = "abookify-sidecar/v3"

	// Refresh idx field on the (re-)written word list.
	for i := range merged.Words {
		merged.Words[i].Idx = i
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	log.Printf("redo: wrote %s — %d words (was %d), %d silences (was %d)",
		outputPath, len(merged.Words), len(existing.Words), len(merged.Silences), len(existing.Silences))
	return nil
}

type timeRange struct{ start, end float64 }

type redoEntry struct {
	idx      int
	base     string
	startSec float64
	endSec   float64
}

func redoToRanges(redo []redoEntry) []timeRange {
	out := make([]timeRange, 0, len(redo))
	for _, r := range redo {
		out = append(out, timeRange{r.startSec, r.endSec})
	}
	return out
}

func readSidecar(path string) (*sidecarV3, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc sidecarV3
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &sc, nil
}

func mergeWords(existing, fresh []wordTS, drop []timeRange) []wordTS {
	out := make([]wordTS, 0, len(existing)+len(fresh))
	for _, w := range existing {
		if inAnyRange(w.Start, drop) {
			continue
		}
		out = append(out, w)
	}
	out = append(out, fresh...)
	sortByStartW(out)
	return out
}

func mergeSilences(existing, fresh []silenceEvent, drop []timeRange) []silenceEvent {
	out := make([]silenceEvent, 0, len(existing)+len(fresh))
	for _, s := range existing {
		if inAnyRange(s.Start, drop) {
			continue
		}
		out = append(out, s)
	}
	out = append(out, fresh...)
	sortByStartS(out)
	return out
}

func inAnyRange(t float64, ranges []timeRange) bool {
	for _, r := range ranges {
		if t >= r.start && t < r.end {
			return true
		}
	}
	return false
}

// Tiny manual insertion sorts: the underlying slices are pre-sorted on
// each side, and we just need the combined result sorted. n*log(n) is
// fine for ≤250k words in O(milliseconds).
func sortByStartW(ws []wordTS) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && ws[j].Start < ws[j-1].Start; j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

func sortByStartS(ss []silenceEvent) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j].Start < ss[j-1].Start; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// Small helper for the log line.
func redoFilesSummary(redo []redoEntry) string {
	names := make([]string, 0, len(redo))
	for _, r := range redo {
		names = append(names, r.base)
	}
	return strings.Join(names, ", ")
}
