package library

import (
	"errors"
	"math"
	"net"
	"testing"
)

// Chunks are cut with `-c copy`, so the segment container must match the input
// codec. Getting this wrong is not a soft failure: ffmpeg refuses to mux a
// copied AAC stream into .mp3 ("Invalid argument") and the whole file fails to
// transcribe. This is what made the library's m4a books impossible to process.
func TestSegmentExt(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/library/book.mp3", ".mp3"},
		{"/library/Animal Farm/01 - Animal Farm.m4a", ".m4a"},
		{"/library/book.m4b", ".m4a"}, // no .m4b muxer — use the MP4-family one
		{"/library/book.mp4", ".m4a"},
		{"/library/book.aac", ".m4a"},
		{"/library/carlin.opus", ".ogg"}, // opus lives in an Ogg container
		{"/library/book.ogg", ".ogg"},
		{"/library/book.oga", ".ogg"},
		{"/library/book.flac", ".flac"},
		{"/library/book.wav", ".wav"},
		{"/library/BOOK.M4A", ".m4a"}, // case-insensitive
		{"/library/noextension", ".mp3"},
	} {
		if got := segmentExt(tc.in); got != tc.want {
			t.Errorf("segmentExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A file whose duration divides exactly by the chunk size must not get a
// trailing phantom segment starting at EOF: ffmpeg emits an empty file, Whisper
// rejects it, and the caller burns its whole retry budget on nothing.
func TestChunkCountNoPhantomSegment(t *testing.T) {
	for _, tc := range []struct {
		dur  float64
		want int
	}{
		{3600.0, 6}, // exactly 60 min — the case that regressed
		{600.0, 1},  // exactly one chunk
		{1200.0, 2},
		{3240.05, 6}, // 54 min — partial last chunk, unchanged
		{60.0, 1},    // shorter than one chunk
		{0.5, 1},
	} {
		got := int(math.Ceil(tc.dur / chunkDurationSecs))
		if got < 1 {
			got = 1
		}
		if got != tc.want {
			t.Errorf("duration %.2fs -> %d segments, want %d", tc.dur, got, tc.want)
		}
		// The last segment must start strictly before the end of the audio.
		if last := float64((got - 1) * chunkDurationSecs); last >= tc.dur && tc.dur > 0 {
			t.Errorf("duration %.2fs: last segment starts at %.0fs, at/past EOF", tc.dur, last)
		}
	}
}

// The two failure classes need opposite responses: a service that cannot be
// REACHED is worth waiting out (a container restart takes minutes), while a
// service that ANSWERS with an error has judged this chunk and will judge it the
// same way again. Conflating them cost 4.3 hours of For Whom the Bell Tolls when
// a whisper redeploy outlasted a 3-attempt/6-second retry budget.
func TestIsServiceUnavailable(t *testing.T) {
	down := []error{
		errors.New(`stt request failed: Post "http://localhost:5200/transcribe": dial tcp 127.0.0.1:5200: connect: connection refused`),
		errors.New(`stt request failed: Post "http://localhost:5200/transcribe": EOF`),
		errors.New(`stt request failed: write tcp 127.0.0.1:59840->127.0.0.1:5200: write: connection reset by peer`),
		errors.New(`Post "http://whisper:5200/transcribe": dial tcp: lookup whisper: no such host`),
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
	}
	for _, e := range down {
		if !isServiceUnavailable(e) {
			t.Errorf("should be treated as service-down (wait it out): %v", e)
		}
	}

	answered := []error{
		// The service is UP and rejected this chunk — retrying identical bytes
		// will not help, and must not hold the run open for minutes.
		errors.New(`stt error (status 500): {"error":"[Errno 1094995529] Invalid data found when processing input"}`),
		errors.New(`stt error (status 500): {"error":"boolean index did not match indexed array along axis 0"}`),
		errors.New(`stt error (status 400): {"error":"missing file field"}`),
		nil,
	}
	for _, e := range answered {
		if isServiceUnavailable(e) {
			t.Errorf("should NOT be treated as service-down: %v", e)
		}
	}
}
