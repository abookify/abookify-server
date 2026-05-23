// Audio waveform peak generation. For each audio file, decode samples
// into N buckets and record the peak amplitude in each. Used by the web
// + mobile UIs to render a waveform visualization under the scrubber.
//
// Caches to disk as JSON so we only decode once per file. Peaks are
// normalized floats in [0, 1]. 2000 peaks per file gives a useful
// visualization at typical screen widths without bloating storage.
package library

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pj/abookify/internal/db"
)

const waveformPeaks = 2000

// Waveform is the cached peak data for one audio file.
type Waveform struct {
	BookID   int64     `json:"book_id"`
	Duration float64   `json:"duration_secs"`
	Peaks    []float32 `json:"peaks"` // normalized 0-1, length = waveformPeaks
}

// WaveformCachePath returns where the waveform JSON is stored for a book.
func WaveformCachePath(generatedDir string, bookID int64) string {
	return filepath.Join(generatedDir, "waveforms", fmt.Sprintf("%d.json", bookID))
}

// GenerateWaveform decodes the audio file via ffmpeg and produces a
// peak-per-bucket waveform. Results are written to the cache path and
// returned. If the cache exists, returns the cached value.
func GenerateWaveform(book db.Book, generatedDir string) (*Waveform, error) {
	if book.MediaType != "audio" {
		return nil, fmt.Errorf("not an audio book")
	}
	cachePath := WaveformCachePath(generatedDir, book.ID)

	// Cache hit?
	if data, err := os.ReadFile(cachePath); err == nil {
		var wf Waveform
		if err := json.Unmarshal(data, &wf); err == nil {
			return &wf, nil
		}
	}

	// ffmpeg: decode to 8kHz mono s16le raw PCM, piped to stdout. Low
	// sample rate is fine — we're computing gross peaks, not fidelity.
	cmd := exec.Command("ffmpeg",
		"-v", "error",
		"-i", book.Path,
		"-ac", "1", // mono
		"-ar", "8000", // 8 kHz
		"-f", "s16le",
		"-",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg decode: %w", err)
	}

	raw := buf.Bytes()
	totalSamples := len(raw) / 2 // 2 bytes per s16 sample
	if totalSamples == 0 {
		return nil, fmt.Errorf("no samples decoded")
	}
	samplesPerBucket := totalSamples / waveformPeaks
	if samplesPerBucket < 1 {
		samplesPerBucket = 1
	}

	peaks := make([]float32, waveformPeaks)
	for i := 0; i < waveformPeaks; i++ {
		start := i * samplesPerBucket
		end := start + samplesPerBucket
		if end > totalSamples {
			end = totalSamples
		}
		var peak int16
		for j := start; j < end; j++ {
			s := int16(binary.LittleEndian.Uint16(raw[j*2 : j*2+2]))
			abs := s
			if abs < 0 {
				abs = -abs
			}
			if abs > peak {
				peak = abs
			}
		}
		peaks[i] = float32(peak) / 32768.0
	}

	wf := &Waveform{
		BookID:   book.ID,
		Duration: float64(totalSamples) / 8000.0,
		Peaks:    peaks,
	}

	// Write cache.
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err == nil {
		if data, err := json.Marshal(wf); err == nil {
			os.WriteFile(cachePath, data, 0644)
		}
	}
	return wf, nil
}

// GenerateWorkWaveform builds a single waveform spanning every audio
// file in the work, ordered the same way the mobile player iterates
// tracks (ListWorks returns books ORDER BY filename). Per-file peaks
// are loaded from GenerateWaveform's cache so this is fast once the
// individual files have been seen; only a brand-new audiobook pays
// the ffmpeg decode cost on first request.
//
// Returns a Waveform with BookID=0, Duration = sum of per-file
// durations, and Peaks = waveformPeaks samples re-bucketed across
// the merged timeline. The output shape matches GenerateWaveform so
// clients can use the same renderer (#180).
func GenerateWorkWaveform(work db.Work, generatedDir string) (*Waveform, error) {
	if !work.HasAudio || len(work.AudioFiles) == 0 {
		return nil, fmt.Errorf("no audio files")
	}
	// Single-file works: forward to the per-file path so renderers and
	// the cache stay shared. BookID is preserved (non-zero) — callers
	// that need to distinguish merged-vs-single can check len(audio_files).
	if len(work.AudioFiles) == 1 {
		return GenerateWaveform(work.AudioFiles[0], generatedDir)
	}

	perFile := make([]*Waveform, 0, len(work.AudioFiles))
	durs := make([]float64, 0, len(work.AudioFiles))
	totalDur := 0.0
	for _, b := range work.AudioFiles {
		wf, err := GenerateWaveform(b, generatedDir)
		if err != nil {
			return nil, fmt.Errorf("waveform for %s: %w", b.Filename, err)
		}
		// Prefer the DB-recorded duration so the merged total agrees with
		// what mobile already sums from book.duration_secs. Fall back to
		// the sample-measured duration when metadata is missing.
		d := b.Duration
		if d <= 0 {
			d = wf.Duration
		}
		perFile = append(perFile, wf)
		durs = append(durs, d)
		totalDur += d
	}
	if totalDur <= 0 {
		return nil, fmt.Errorf("zero total duration")
	}

	starts := make([]float64, len(perFile))
	cum := 0.0
	for i, d := range durs {
		starts[i] = cum
		cum += d
	}

	out := make([]float32, waveformPeaks)
	for k := 0; k < waveformPeaks; k++ {
		gStart := float64(k) * totalDur / float64(waveformPeaks)
		gEnd := float64(k+1) * totalDur / float64(waveformPeaks)
		var peak float32
		for i, wf := range perFile {
			bookStart := starts[i]
			bookEnd := bookStart + durs[i]
			if gEnd <= bookStart || gStart >= bookEnd {
				continue
			}
			lStart := math.Max(0, gStart-bookStart)
			lEnd := math.Min(durs[i], gEnd-bookStart)
			if lEnd <= lStart {
				continue
			}
			jStart := int(math.Floor(lStart / durs[i] * float64(waveformPeaks)))
			jEnd := int(math.Ceil(lEnd / durs[i] * float64(waveformPeaks)))
			if jStart < 0 {
				jStart = 0
			}
			if jEnd > waveformPeaks {
				jEnd = waveformPeaks
			}
			if jStart >= jEnd {
				// Output bucket smaller than a per-file bucket — take the
				// single sample at jStart so we never produce a hole.
				jEnd = jStart + 1
				if jEnd > waveformPeaks {
					continue
				}
			}
			for j := jStart; j < jEnd; j++ {
				if wf.Peaks[j] > peak {
					peak = wf.Peaks[j]
				}
			}
		}
		out[k] = peak
	}

	return &Waveform{
		BookID:   0,
		Duration: totalDur,
		Peaks:    out,
	}, nil
}

// normalizePeaks stretches the peak range to [0, 1] if the loudest sample
// is well below full-scale. Improves visual contrast on quiet recordings.
func normalizePeaks(peaks []float32) {
	var max float32
	for _, p := range peaks {
		if p > max {
			max = p
		}
	}
	if max <= 0 {
		return
	}
	scale := float32(1.0 / math.Max(float64(max), 0.01))
	for i := range peaks {
		peaks[i] *= scale
	}
}
