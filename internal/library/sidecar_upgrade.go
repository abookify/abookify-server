// Sidecar upgrade chain: v1 / v2 → v3.
//
// The shape changes between versions are additive — v2 added silences and
// sources to v1, v3 namespaces all derived data under metadata{} — so
// transforms are mechanical and never lossy. Versioned upgrades let the
// server consume any historic sidecar without growing N read paths.
package library

import (
	"encoding/json"
	"fmt"
	"time"
)

// legacySidecar is the union of v1 + v2 fields. Used as a single decode
// target during upgrade so we don't need separate v1Sidecar/v2Sidecar
// types — fields absent in v1 just stay zero-valued.
type legacySidecar struct {
	Version  int           `json:"version"` // 0 (v1) or 2
	Language string        `json:"language"`
	Duration float64       `json:"duration"`
	Text     string        `json:"text"`
	Words    []legacyWord  `json:"words"`
	Silences []legacySilence `json:"silences"`
	Sources  []legacySource  `json:"sources"`
	Chapters []legacyChapter `json:"chapters"`
}

type legacyWord struct {
	Start       float64 `json:"s"`
	End         float64 `json:"e"`
	Word        string  `json:"w"`
	Probability float64 `json:"conf,omitempty"`
}

type legacySilence struct {
	Start    float64 `json:"s"`
	End      float64 `json:"e"`
	Duration float64 `json:"duration"`
	Source   string  `json:"source"`
	RmsDB    float64 `json:"rms_db,omitempty"`
	Kind     string  `json:"kind"`
}

type legacySource struct {
	File         string  `json:"file"`
	OffsetSecs   float64 `json:"offset_secs"`
	DurationSecs float64 `json:"duration_secs"`
}

type legacyChapter struct {
	Title     string  `json:"title"`
	Start     float64 `json:"start_sec"`
	End       float64 `json:"end_sec"`
	WordIdx   int     `json:"word_idx"`
	WordCount int     `json:"word_count"`
	Src       string  `json:"src,omitempty"`
}

// UpgradeToV3 converts a v1 or v2 sidecar payload (raw JSON bytes) to a
// v3 struct. The transform is purely structural — no audio decoding,
// no LLM calls, no algorithm reruns. Server can call this at read time
// with negligible overhead.
//
// Migration rules:
//   - v1 had no "version" field. Words, language, duration, text are
//     copied as-is. Silences and sources don't exist in v1 (server
//     post-process will derive them lazily if it needs them).
//   - v2 added silences[] and sources[] alongside the v1 fields.
//   - In both, chapters[] (if present) was written by stt-cli's
//     -detect-chapters flag — but the server already prefers re-running
//     DetectChapters on the word stream when narrator-pattern is strong.
//     So we move v1/v2 chapters[] into metadata.chapters with source
//     tagged "legacy:stt-cli-pre-v3", and the server's import pipeline
//     will overwrite this with its own pass on next run.
func UpgradeToV3(data []byte) (*SidecarV3, error) {
	var legacy legacySidecar
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("parse legacy sidecar: %w", err)
	}

	v3 := &SidecarV3{
		Version:  SchemaVersion,
		Schema:   "abookify-sidecar/v3",
		Language: legacy.Language,
		Duration: legacy.Duration,
	}

	// Copy atomic outputs as-is.
	v3.Words = make([]SidecarWord, len(legacy.Words))
	for i, w := range legacy.Words {
		v3.Words[i] = SidecarWord{
			Start:       w.Start,
			End:         w.End,
			Word:        w.Word,
			Probability: w.Probability,
		}
	}
	v3.Silences = make([]SidecarSilence, len(legacy.Silences))
	for i, s := range legacy.Silences {
		v3.Silences[i] = SidecarSilence{
			Start:    s.Start,
			End:      s.End,
			Duration: s.Duration,
			Source:   s.Source,
			RmsDB:    s.RmsDB,
			Kind:     s.Kind,
		}
	}
	v3.Sources = make([]SidecarSource, len(legacy.Sources))
	for i, src := range legacy.Sources {
		v3.Sources[i] = SidecarSource{
			File:         src.File,
			OffsetSecs:   src.OffsetSecs,
			DurationSecs: src.DurationSecs,
		}
	}

	// Move legacy chapters into metadata.chapters tagged as legacy so the
	// server's chapter-detection pass knows to overwrite them.
	if len(legacy.Chapters) > 0 {
		entries := make([]SidecarChapter, len(legacy.Chapters))
		for i, c := range legacy.Chapters {
			kind := c.Src
			if kind == "" {
				kind = "chapter"
			}
			entries[i] = SidecarChapter{
				Index:     i,
				Title:     c.Title,
				Kind:      kind,
				StartSec:  c.Start,
				EndSec:    c.End,
				WordIdx:   c.WordIdx,
				WordCount: c.WordCount,
				Source:    "legacy:stt-cli-pre-v3",
			}
		}
		v3.Metadata.Chapters = &ChapterSection{
			SectionMeta: SectionMeta{
				Algo:       "legacy:stt-cli-pre-v3",
				ComputedAt: time.Now().UTC().Format(time.RFC3339),
			},
			Entries: entries,
		}
	}

	return v3, nil
}
