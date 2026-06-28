package library

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pj/abookify/internal/stt"
)

const chunkDurationSecs = 600 // 10 minutes per segment

// SegmentEvent reports per-segment progress from ChunkedTranscribe. It fires
// twice per segment: once before the Whisper call (Done=false, for progress
// UIs) and once after (Done=true, with Words/RealtimeX for ETA logging).
type SegmentEvent struct {
	SegIdx       int     // 0-based segment index within this file
	TotalSegs    int     // total segments in this file
	SegStartSecs int     // segment start offset within this file
	Done         bool    // false = about to transcribe; true = finished
	Words        int     // words transcribed in this segment (Done only)
	Failed       bool    // segment failed permanently after retries (Done only)
}

// ChunkedTranscribe is the single shared long-audio transcription primitive used
// by BOTH the server's generation pipeline and stt-cli — neither carries its own
// copy. It splits audioPath into 10-minute segments, transcribes each via the
// Whisper HTTP client (with retry/backoff on transient failures, continuing past
// a permanently-failed segment rather than aborting the file), and stitches the
// results into one timeline. All timestamps are shifted by baseOffset (the prior
// files' cumulative duration in a multi-file book; pass 0 for a standalone file).
func ChunkedTranscribe(client *stt.Client, audioPath string, baseOffset float64, onSeg func(SegmentEvent)) (*stt.TranscribeResult, error) {
	dur := probeDurationFile(audioPath)
	if dur <= 0 {
		return nil, fmt.Errorf("could not determine audio duration for %s", audioPath)
	}

	nSegments := int(dur/chunkDurationSecs) + 1

	tmpDir, err := os.MkdirTemp("", "abookify-stt-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var combined stt.TranscribeResult
	combined.Duration = dur

	for i := 0; i < nSegments; i++ {
		startSecs := i * chunkDurationSecs
		segPath := filepath.Join(tmpDir, fmt.Sprintf("seg-%04d.mp3", i))

		// ffmpeg segment extraction (copy codec — fast, no re-encode).
		cmd := exec.Command("ffmpeg", "-y", "-v", "error",
			"-ss", strconv.Itoa(startSecs), "-t", strconv.Itoa(chunkDurationSecs),
			"-i", audioPath, "-c", "copy", segPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg split segment %d: %v\n%s", i, err, string(out))
		}

		if onSeg != nil {
			onSeg(SegmentEvent{SegIdx: i, TotalSegs: nSegments, SegStartSecs: startSecs})
		}

		// Retry transient Whisper failures (intermittent 500s on a segment that
		// succeeds when retried). After maxAttempts, continue with an empty
		// result so one bad chunk doesn't lose the rest of a multi-hour file.
		const maxAttempts = 3
		var result *stt.TranscribeResult
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			result, err = client.TranscribeFile(segPath)
			if err == nil {
				break
			}
			if attempt < maxAttempts {
				backoff := time.Duration(attempt*2) * time.Second
				log.Printf("stt-chunked: segment %d attempt %d/%d failed (%v); retry in %v",
					i+1, attempt, maxAttempts, err, backoff)
				time.Sleep(backoff)
			}
		}
		os.Remove(segPath)
		failed := err != nil
		if failed {
			log.Printf("stt-chunked: segment %d failed permanently after %d attempts: %v — skipping",
				i+1, maxAttempts, err)
			result = &stt.TranscribeResult{}
		}

		// Merge: shift all timestamps by baseOffset + this segment's start.
		offset := baseOffset + float64(startSecs)
		for _, seg := range result.Segments {
			shifted := stt.Segment{Start: seg.Start + offset, End: seg.End + offset, Text: seg.Text}
			for _, w := range seg.Words {
				shifted.Words = append(shifted.Words, stt.Word{
					Word: w.Word, Start: w.Start + offset, End: w.End + offset, Probability: w.Probability,
				})
			}
			combined.Segments = append(combined.Segments, shifted)
		}
		combined.Text += result.Text + " "
		if combined.Language == "" {
			combined.Language = result.Language
			combined.LanguageProbability = result.LanguageProbability
		}

		if onSeg != nil {
			onSeg(SegmentEvent{SegIdx: i, TotalSegs: nSegments, SegStartSecs: startSecs,
				Done: true, Words: len(strings.Fields(result.Text)), Failed: failed})
		}
	}

	combined.Text = strings.TrimSpace(combined.Text)
	return &combined, nil
}

// transcribeChunked is the server-side adapter over the shared primitive.
// onProgress fires once per segment (before transcription) to drive the job UI.
func transcribeChunked(client *stt.Client, audioPath string, onProgress func(segIdx, totalSegs int)) (*stt.TranscribeResult, error) {
	return ChunkedTranscribe(client, audioPath, 0, func(e SegmentEvent) {
		if e.Done {
			log.Printf("stt-chunked: segment %d/%d done (offset=%ds, %d words)",
				e.SegIdx+1, e.TotalSegs, e.SegStartSecs, e.Words)
			return
		}
		if onProgress != nil {
			onProgress(e.SegIdx, e.TotalSegs)
		}
	})
}

// probeDurationFile returns the duration in seconds via ffprobe (0 on error).
func probeDurationFile(path string) float64 {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}
