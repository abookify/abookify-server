package library

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pj/abookify/internal/stt"
)

const chunkDurationSecs = 600 // 10 minutes per segment

// transcribeChunked splits a large audio file into 10-minute segments, transcribes
// each via Whisper, then stitches the results together with offset-corrected timestamps.
// Returns the combined transcript result (all segments merged).
//
// The onProgress callback fires after each segment so the caller can update the UI.
func transcribeChunked(client *stt.Client, audioPath string, onProgress func(segIdx, totalSegs int)) (*stt.TranscribeResult, error) {
	dur := probeDurationFile(audioPath)
	if dur <= 0 {
		return nil, fmt.Errorf("could not determine audio duration for %s", audioPath)
	}

	nSegments := int(dur/chunkDurationSecs) + 1
	if nSegments <= 1 {
		// Short file — transcribe directly.
		return client.TranscribeFile(audioPath)
	}

	tmpDir, err := os.MkdirTemp("", "abookify-stt-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	log.Printf("stt-chunked: splitting %.0fs audio into %d segments of %ds", dur, nSegments, chunkDurationSecs)

	var combined stt.TranscribeResult
	combined.Duration = dur

	for i := 0; i < nSegments; i++ {
		startSecs := i * chunkDurationSecs
		segPath := filepath.Join(tmpDir, fmt.Sprintf("seg-%04d.mp3", i))

		// ffmpeg segment extraction (copy codec — fast, no re-encode)
		args := []string{
			"-y", "-v", "error",
			"-ss", strconv.Itoa(startSecs),
			"-t", strconv.Itoa(chunkDurationSecs),
			"-i", audioPath,
			"-c", "copy",
			segPath,
		}
		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ffmpeg split segment %d: %v\n%s", i, err, string(out))
		}

		if onProgress != nil {
			onProgress(i, nSegments)
		}

		result, err := client.TranscribeFile(segPath)
		if err != nil {
			return nil, fmt.Errorf("transcribe segment %d (start=%ds): %w", i, startSecs, err)
		}

		// Merge: offset all timestamps by the segment's start time.
		offset := float64(startSecs)
		for _, seg := range result.Segments {
			shifted := stt.Segment{
				Start: seg.Start + offset,
				End:   seg.End + offset,
				Text:  seg.Text,
			}
			for _, w := range seg.Words {
				shifted.Words = append(shifted.Words, stt.Word{
					Word:        w.Word,
					Start:       w.Start + offset,
					End:         w.End + offset,
					Probability: w.Probability,
				})
			}
			combined.Segments = append(combined.Segments, shifted)
		}
		combined.Text += result.Text + " "

		if combined.Language == "" {
			combined.Language = result.Language
			combined.LanguageProbability = result.LanguageProbability
		}

		log.Printf("stt-chunked: segment %d/%d done (offset=%ds, %d words)",
			i+1, nSegments, startSecs, len(strings.Fields(result.Text)))

		// Remove segment immediately to free disk.
		os.Remove(segPath)
	}

	combined.Text = strings.TrimSpace(combined.Text)
	return &combined, nil
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
