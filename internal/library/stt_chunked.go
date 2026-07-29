package library

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
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
	SegIdx       int  // 0-based segment index within this file
	TotalSegs    int  // total segments in this file
	SegStartSecs int  // segment start offset within this file
	Done         bool // false = about to transcribe; true = finished
	Words        int  // words transcribed in this segment (Done only)
	Failed       bool // segment failed permanently after retries (Done only)
}

// ChunkedTranscribe is the single shared long-audio transcription primitive used
// by BOTH the server's generation pipeline and stt-cli — neither carries its own
// copy. It splits audioPath into 10-minute segments, transcribes each via the
// Whisper HTTP client (with retry/backoff on transient failures, continuing past
// a permanently-failed segment rather than aborting the file), and stitches the
// results into one timeline. All timestamps are shifted by baseOffset (the prior
// files' cumulative duration in a multi-file book; pass 0 for a standalone file).
func ChunkedTranscribe(client stt.Provider, audioPath string, baseOffset float64, onSeg func(SegmentEvent)) (*stt.TranscribeResult, error) {
	dur := probeDurationFile(audioPath)
	if dur <= 0 {
		return nil, fmt.Errorf("could not determine audio duration for %s", audioPath)
	}

	// ceil, NOT int()+1: a file whose duration is an exact multiple of the chunk
	// size (a 60.0-minute part, which is how most ripped audiobooks are cut) got
	// one segment too many, starting exactly AT the end. ffmpeg then produced an
	// empty file and Whisper rejected it — "Invalid data found when processing
	// input" — so every such file burned all 3 retry attempts plus backoff on
	// nothing, and logged an alarming "failed permanently" that looked like data
	// loss. Nothing was ever lost (the phantom segment is past the end), but it
	// cost ~40s per file and buried real failures in noise.
	nSegments := int(math.Ceil(dur / chunkDurationSecs))
	if nSegments < 1 {
		nSegments = 1
	}

	tmpDir, err := os.MkdirTemp("", "abookify-stt-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var combined stt.TranscribeResult
	combined.Duration = dur

	for i := 0; i < nSegments; i++ {
		startSecs := i * chunkDurationSecs
		// Skip a trailing sliver. ceil() fixes the exact-multiple case, but a
		// duration a hair OVER a multiple (3600.013s — how a ripped 60-minute
		// part actually measures) still yields a final segment of a few
		// MILLISECONDS. ffmpeg writes it, Whisper rejects it as invalid data,
		// and the run burns its retry budget on audio too short to hold a
		// syllable. Anything under a second cannot contain speech.
		if remaining := dur - float64(startSecs); remaining < minSegmentSecs {
			break
		}
		segPath := filepath.Join(tmpDir, fmt.Sprintf("seg-%04d%s", i, segmentExt(audioPath)))

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

		// Retry, but treat the two failure classes differently — they need
		// opposite responses and conflating them loses hours of work.
		//
		//  - The SERVICE IS GONE (connection refused / EOF / reset). Nothing is
		//    wrong with the audio; something restarted the container. Wait it
		//    out. A redeploy takes minutes, so the old 3 attempts over ~6s could
		//    never survive one: a whisper redeploy mid-run silently emptied FIVE
		//    consecutive hour-long files of For Whom the Bell Tolls (4.3 h) while
		//    the audio was perfectly fine.
		//  - The MODEL REJECTED THIS CHUNK (an HTTP error came back). The service
		//    is up and answering; retrying the identical bytes rarely helps, so
		//    give up quickly and keep the rest of the book.
		var result *stt.TranscribeResult
		attempts := 0
		for {
			attempts++
			result, err = client.TranscribeFile(segPath)
			if err == nil {
				break
			}
			down := isServiceUnavailable(err)
			maxAttempts := segDecodeAttempts
			if down {
				maxAttempts = segServiceAttempts
			}
			if attempts >= maxAttempts {
				break
			}
			backoff := time.Duration(attempts*2) * time.Second
			if down && backoff > serviceRetryCap {
				backoff = serviceRetryCap
			}
			log.Printf("stt-chunked: segment %d attempt %d/%d failed (%v); retry in %v",
				i+1, attempts, maxAttempts, err, backoff)
			time.Sleep(backoff)
		}
		os.Remove(segPath)
		failed := err != nil
		if failed {
			log.Printf("stt-chunked: segment %d failed permanently after %d attempts: %v — skipping",
				i+1, attempts, err)
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
func transcribeChunked(client stt.Provider, audioPath string, onProgress func(segIdx, totalSegs int)) (*stt.TranscribeResult, error) {
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

const (
	// Shortest trailing chunk worth sending. Below this it is rounding dust from
	// the container's duration, not audio.
	minSegmentSecs = 1.0
	// A chunk the model itself rejects is usually deterministic — fail fast.
	segDecodeAttempts = 3
	// A chunk that cannot reach the service is worth waiting out: 20 attempts
	// capped at 30s is ~9 minutes, comfortably longer than a container rebuild
	// and model reload, and it costs nothing when the service is healthy.
	segServiceAttempts = 20
	serviceRetryCap    = 30 * time.Second
)

// isServiceUnavailable reports whether the STT call failed because the service
// could not be REACHED, as opposed to answering with an error. Only the former
// is worth waiting out.
func isServiceUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// A reachable service that returns an HTTP status is not "down", even for a
	// 5xx — the client reports those as "stt error (status N)".
	if strings.Contains(err.Error(), "stt error (status") {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection refused", "connection reset", "EOF",
		"no such host", "broken pipe", "i/o timeout"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// segmentExt picks the container for an extracted chunk.
//
// It has to MATCH THE INPUT: segments are cut with `-c copy` (no re-encode, so
// splitting a 17-hour book costs seconds), and ffmpeg refuses to mux a copied
// AAC stream into a .mp3 container — "Error opening output files: Invalid
// argument". Hardcoding .mp3 silently worked only because every book had been
// MP3 until m4a and opus arrived; it made the library's 44.5 hours of m4a
// (Animal Farm, the Dan Carlin epics) impossible to transcribe.
//
// A few extensions name a container ffmpeg has no muxer for, so they map to the
// equivalent it does know.
func segmentExt(audioPath string) string {
	ext := strings.ToLower(filepath.Ext(audioPath))
	switch ext {
	case ".m4b", ".mp4", ".aac", ".m4a":
		return ".m4a" // one MP4-family muxer for the lot
	case ".oga", ".opus", ".ogg":
		return ".ogg"
	case "":
		return ".mp3" // no extension to go on; MP3 is the library's default
	default:
		return ext // .mp3, .flac, .wav, … ffmpeg infers the muxer
	}
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
