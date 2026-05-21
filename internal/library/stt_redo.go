// Server-side equivalent of `stt-cli --redo-files`: re-transcribe a
// subset of a work's audio files and merge the new words back into
// the existing sidecar. Triggered from the gap-detection UI so the
// user can fix Whisper failures without dropping to a CLI.
//
// Flow:
//   1. Read the existing sidecar from disk.
//   2. For each filename requested, find the matching audio book in
//      the work, run transcribeChunked against it, shift timestamps
//      onto the concatenated timeline using the sidecar's sources[]
//      offsets.
//   3. Merge: drop existing words whose Start falls in any redone
//      file's time range, append the new words, re-sort.
//   4. Write the sidecar back, then call ReimportWork to refresh the
//      DB rows (chapters, paragraphs, transcription_gaps).
//
// Silences for the redone range are intentionally NOT recomputed —
// silencedetect already ran during the original sidecar build for
// every audio second, including the seconds where Whisper failed.
// Reusing those silences avoids needing ffmpeg here and keeps the
// chapter-detection signal intact.
package library

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
	"github.com/pj/abookify/internal/stt"
)

// rawSidecar is the on-disk shape we read + write. Only the fields we
// need are typed; everything else passes through as json.RawMessage so
// we don't accidentally drop forward-compatible additions.
type rawSidecar struct {
	Version  int               `json:"version"`
	Schema   string            `json:"schema"`
	Language string            `json:"language,omitempty"`
	Duration float64           `json:"duration"`
	Sources  []sttSource       `json:"sources,omitempty"`
	Words    []sttWord         `json:"words"`
	Silences []sttSilence      `json:"silences,omitempty"`
	Metadata json.RawMessage   `json:"metadata,omitempty"`
}

// redoTranscriptionForFiles is the workhorse called by the queue.
// onProgress is invoked between segments so the job UI can render a
// bar. Returns the number of files actually retranscribed.
func redoTranscriptionForFiles(
	store *db.Store,
	sttClient *stt.Client,
	libraryRoot string,
	workID int64,
	filenames []string,
	onProgress func(fileIdx, fileCount int, fileName string, segIdx, totalSegs int),
) (int, error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, fmt.Errorf("work %d not found", workID)
	}
	if len(work.AudioFiles) == 0 {
		return 0, fmt.Errorf("work %d has no audio", workID)
	}

	sidecarPath := findSidecar(work.AudioFiles[0].Path, libraryRoot)
	if sidecarPath == "" {
		return 0, fmt.Errorf("no sidecar for work %d", workID)
	}
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return 0, fmt.Errorf("read sidecar: %w", err)
	}
	var sc rawSidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return 0, fmt.Errorf("parse sidecar: %w", err)
	}

	// Index source files by basename so we can map filename → time range.
	type fileSpan struct {
		path     string
		startSec float64
		endSec   float64
	}
	byName := map[string]fileSpan{}
	if len(sc.Sources) > 0 {
		// Multi-file book: timeline offsets come from the sidecar.
		// Map each source.filename to the path on disk by matching
		// against the work's audio_files (which carry the full path).
		pathByBase := map[string]string{}
		for _, af := range work.AudioFiles {
			pathByBase[filepath.Base(af.Path)] = af.Path
		}
		for _, src := range sc.Sources {
			byName[src.Filename] = fileSpan{
				path:     pathByBase[src.Filename],
				startSec: src.StartSec,
				endSec:   src.StartSec + src.Duration,
			}
		}
	} else {
		// Single-file book: timeline is just [0, duration).
		af := work.AudioFiles[0]
		byName[filepath.Base(af.Path)] = fileSpan{
			path:     af.Path,
			startSec: 0,
			endSec:   sc.Duration,
		}
	}

	// Validate every requested filename + collect the spans to redo.
	type redoTarget struct {
		name string
		span fileSpan
	}
	var targets []redoTarget
	for _, name := range filenames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		span, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("file %q not in work's sources", name)
		}
		if span.path == "" {
			return 0, fmt.Errorf("file %q referenced in sidecar but missing from work", name)
		}
		targets = append(targets, redoTarget{name: name, span: span})
	}
	if len(targets) == 0 {
		return 0, fmt.Errorf("no files specified")
	}

	// Transcribe each target and accumulate fresh words on the global
	// timeline. We deliberately re-use the existing chunked path so
	// retry behavior + progress reporting match a normal STT job.
	var freshWords []sttWord
	dropRanges := make([]timeRange, 0, len(targets))
	for i, t := range targets {
		log.Printf("redo-stt: transcribing %s (%.0fs-%.0fs)", t.name, t.span.startSec, t.span.endSec)
		result, err := transcribeChunked(sttClient, t.span.path, func(segIdx, totalSegs int) {
			if onProgress != nil {
				onProgress(i, len(targets), t.name, segIdx, totalSegs)
			}
		})
		if err != nil {
			return 0, fmt.Errorf("transcribe %s: %w", t.name, err)
		}
		shift := t.span.startSec
		for _, seg := range result.Segments {
			for _, w := range seg.Words {
				freshWords = append(freshWords, sttWord{
					Start: w.Start + shift,
					End:   w.End + shift,
					Word:  w.Word,
				})
			}
		}
		dropRanges = append(dropRanges, timeRange{start: t.span.startSec, end: t.span.endSec})
	}

	// Merge: keep existing words that aren't in any redo range, then
	// append the fresh ones and re-sort by Start.
	merged := make([]sttWord, 0, len(sc.Words)+len(freshWords))
	for _, w := range sc.Words {
		if inAnyTimeRange(w.Start, dropRanges) {
			continue
		}
		merged = append(merged, w)
	}
	merged = append(merged, freshWords...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Start < merged[j].Start })
	sc.Words = merged

	out, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal sidecar: %w", err)
	}
	// Write atomically: same dir, .tmp, rename. Crashes mid-write
	// otherwise corrupt the sidecar and lose hours of prior work.
	tmp := sidecarPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return 0, fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, sidecarPath); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("rename: %w", err)
	}
	log.Printf("redo-stt: merged %d new words into %s (%d total)",
		len(freshWords), sidecarPath, len(merged))

	// Re-run the full post-processing pipeline so chapter detection,
	// paragraphs, and transcription_gaps all reflect the new words.
	if err := ReimportWork(store, workID, libraryRoot); err != nil {
		return len(targets), fmt.Errorf("reimport after merge: %w", err)
	}
	return len(targets), nil
}

type timeRange struct{ start, end float64 }

func inAnyTimeRange(t float64, ranges []timeRange) bool {
	for _, r := range ranges {
		if t >= r.start && t < r.end {
			return true
		}
	}
	return false
}
