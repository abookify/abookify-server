package library

import "testing"

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
