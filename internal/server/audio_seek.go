package server

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// frameOffsetAtTime resolves an accurate MP3 frame boundary at or just before t
// (seconds), returning the byte offset of that frame and its true timestamp.
//
// This is the server half of "seek B" (see docs/seek-index-design.md): a
// headerless VBR MP3 carries no time→byte index, so ExoPlayer and the browser
// estimate the seek from average bitrate and land seconds off. ffmpeg, unlike
// them, PARSES frames to seek, so it lands exactly — verified on real VBR content
// (the frame at 1800.000s resolves to its true byte in ~0.06s). The client seeks
// by reloading the stream at ?t=; the server anchors it to a real frame here.
//
// Returns ok=false when t is past the end of the audio (ffprobe yields no packet)
// or the file cannot be read — the caller answers 416 for that.
func frameOffsetAtTime(path string, t float64) (startByte int64, actualSec float64, ok bool) {
	if t < 0 {
		t = 0
	}
	// A short window bracketing t; each audio packet in it carries pts_time + byte
	// pos. Start a hair before t so we can pick the frame AT OR BEFORE t (never
	// overshoot the requested position); fall back to the earliest frame in the
	// window if t sits before it.
	lo := t - 0.10
	if lo < 0 {
		lo = 0
	}
	iv := fmt.Sprintf("%.3f%%+0.30", lo)
	out, err := exec.Command("ffprobe", "-v", "error",
		"-read_intervals", iv,
		"-select_streams", "a:0",
		"-show_entries", "packet=pts_time,pos",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, 0, false
	}
	var (
		bestByte  int64 = -1
		bestSec   float64
		firstByte int64 = -1
		firstSec  float64
	)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) != 2 {
			continue
		}
		ts, e1 := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64)
		pos, e2 := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if e1 != nil || e2 != nil || pos < 0 {
			continue
		}
		if firstByte < 0 {
			firstByte, firstSec = pos, ts
		}
		if ts <= t && (bestByte < 0 || ts > bestSec) {
			bestByte, bestSec = pos, ts
		}
	}
	if bestByte >= 0 {
		// Past-end guard: ffprobe clamps a seek beyond EOF to the file's tail and
		// returns those frames, so a bestSec far below t means t is past the audio
		// (the frames are self-consistent but not at t). Frames are ~tens of ms
		// apart, so a legitimate hit sits within the window of t; > 0.5 s below is
		// unambiguously past-end (or a seek that landed nowhere near t).
		if t-bestSec > 0.5 {
			return 0, 0, false
		}
		return bestByte, bestSec, true
	}
	if firstByte >= 0 {
		return firstByte, firstSec, true // t precedes the first frame in the window
	}
	return 0, 0, false
}
