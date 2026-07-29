package library

import (
	"errors"
	"math"
	"net"
	"testing"

	"github.com/pj/abookify/internal/stt"
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

	// A duration a hair OVER a multiple is the case ceil() alone does not fix:
	// a ripped 60-minute part measures 3600.013s, so segment 7 would carry 13
	// milliseconds. The loop must skip any trailing sliver under minSegmentSecs.
	for _, dur := range []float64{3600.013, 3599.987, 1200.004, 600.0009} {
		n := int(math.Ceil(dur / chunkDurationSecs))
		emitted := 0
		for i := 0; i < n; i++ {
			if dur-float64(i*chunkDurationSecs) < minSegmentSecs {
				break
			}
			emitted++
		}
		lastStart := float64((emitted - 1) * chunkDurationSecs)
		if tail := dur - lastStart; tail < minSegmentSecs {
			t.Errorf("duration %.4fs: emitted a %.3fs trailing sliver", dur, tail)
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

// fakeProvider reports a device that the test can change underneath a run,
// which is exactly what a `docker compose up` without the CUDA overlay does.
type fakeProvider struct{ device string }

func (f *fakeProvider) TranscribeFile(string) (*stt.TranscribeResult, error) {
	return &stt.TranscribeResult{}, nil
}
func (f *fakeProvider) Name() string  { return "fake" }
func (f *fakeProvider) Health() error { return nil }
func (f *fakeProvider) Info() (*stt.Info, error) {
	return &stt.Info{Device: f.device}, nil
}

// A boot-time check cannot catch a device that flips mid-run — all three GPU
// incidents did exactly that, after any start-up guard had already passed. The
// watcher must notice, and must NOT abort: the run still progresses, checkpoints
// protect completed files, and killing a 23-hour transcription over a
// recoverable misconfiguration would be worse than finishing slowly.
func TestWatchDeviceDetectsMidRunChange(t *testing.T) {
	p := &fakeProvider{device: "cuda"}
	seen := currentDevice(p)
	if seen != "cuda" {
		t.Fatalf("initial device = %q, want cuda", seen)
	}

	// Unchanged: no state churn.
	watchDevice(p, &seen, 1)
	if seen != "cuda" {
		t.Errorf("device tracking drifted while unchanged: %q", seen)
	}

	// The incident: whisper restarts without the GPU.
	p.device = "cpu"
	watchDevice(p, &seen, 7)
	if seen != "cpu" {
		t.Errorf("watcher did not adopt the new device: %q", seen)
	}

	// Adopted, so it reports once rather than every segment thereafter.
	watchDevice(p, &seen, 8)
	if seen != "cpu" {
		t.Errorf("device tracking unstable after change: %q", seen)
	}
}

// A provider that cannot report a device (a bare Provider, or an old service)
// must not break the run.
func TestWatchDeviceTolerantOfNoDeviceInfo(t *testing.T) {
	seen := ""
	watchDevice(nopProvider{}, &seen, 1) // must not panic
	if got := currentDevice(nopProvider{}); got != "" {
		t.Errorf("device for a non-reporting provider = %q, want empty", got)
	}
}

type nopProvider struct{}

func (nopProvider) TranscribeFile(string) (*stt.TranscribeResult, error) {
	return &stt.TranscribeResult{}, nil
}
func (nopProvider) Name() string             { return "nop" }
func (nopProvider) Health() error            { return nil }
func (nopProvider) Info() (*stt.Info, error) { return nil, errors.New("no info") }
