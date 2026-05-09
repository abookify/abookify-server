package main

import (
	"bufio"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
)

// silenceEvent is a real acoustic silence measured by ffmpeg silencedetect.
// Independent of Whisper — reads the waveform directly.
type silenceEvent struct {
	Start    float64 `json:"s"`
	End      float64 `json:"e"`
	Duration float64 `json:"duration"`
	Source   string  `json:"source"`  // "silencedetect" | "vad" | "both"
	RmsDB   float64 `json:"rms_db,omitempty"`
	Kind     string  `json:"kind"`    // classified later: chapter/paragraph/sentence/breath
}

var (
	silenceStartRe = regexp.MustCompile(`silence_start:\s+([\d.]+)`)
	silenceEndRe   = regexp.MustCompile(`silence_end:\s+([\d.]+)\s*\|\s*silence_duration:\s+([\d.]+)`)
)

// detectSilences runs ffmpeg silencedetect on an audio file and returns
// every detected silence interval. The noise threshold (dB) and minimum
// duration (seconds) are configurable.
//
// For audiobooks we want to catch even short breath-pauses (0.15s) at a
// moderate threshold (-30dB). These micro-silences are paragraph/sentence
// markers — chapter breaks are longer but we want ALL silences in the
// event stream so downstream can classify by duration.
func detectSilences(audioPath string, noisedB float64, minDuration float64, baseOffset float64) ([]silenceEvent, error) {
	// Build the audio filter. We pipe through a highpass at 80Hz first to
	// remove room rumble that can mask real pauses, then run silencedetect.
	af := fmt.Sprintf("highpass=f=80, silencedetect=noise=%ddB:d=%.3f", int(noisedB), minDuration)

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-v", "info",
		"-i", audioPath,
		"-af", af,
		"-f", "null", "-",
	)

	// silencedetect writes to stderr (it's an ffmpeg filter diagnostic).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	var silences []silenceEvent
	var pendingStart float64
	hasPending := false

	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if m := silenceStartRe.FindStringSubmatch(line); m != nil {
			s, _ := strconv.ParseFloat(m[1], 64)
			pendingStart = s + baseOffset
			hasPending = true
		} else if m := silenceEndRe.FindStringSubmatch(line); m != nil {
			e, _ := strconv.ParseFloat(m[1], 64)
			d, _ := strconv.ParseFloat(m[2], 64)
			if hasPending {
				silences = append(silences, silenceEvent{
					Start:    pendingStart,
					End:      e + baseOffset,
					Duration: d,
					Source:   "silencedetect",
				})
				hasPending = false
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		// ffmpeg returns non-zero if the input is truncated or has decode
		// warnings, but the silencedetect output is usually still usable.
		// Log but don't fail.
		log.Printf("silencedetect warning: ffmpeg exited with %v (results may be partial)", err)
	}

	return silences, nil
}

// classifySilences sets the Kind field based on duration thresholds.
// Called after all silences are collected.
func classifySilences(silences []silenceEvent) {
	for i := range silences {
		d := silences[i].Duration
		switch {
		case d >= 3.0:
			silences[i].Kind = "chapter"
		case d >= 0.6:
			silences[i].Kind = "paragraph"
		case d >= 0.3:
			silences[i].Kind = "sentence"
		default:
			silences[i].Kind = "breath"
		}
	}
}

// v2 had a merged event stream (interleaved words+silences) — retired
// in v3. Server consumes words[] and silences[] directly, deriving any
// post-processing (chapters, paragraphs, etc.) from those two arrays.
