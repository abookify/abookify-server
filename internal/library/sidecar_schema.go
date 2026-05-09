// Sidecar schema v3 — sectioned metadata for cheap post-processing reruns.
//
// Layered design:
//   - Atomic outputs (words, silences) are expensive — Whisper costs hours,
//     ffmpeg silencedetect costs minutes. They get cached forever.
//   - Derived metadata (chapters, paragraphs, characters, summaries) is
//     cheap — milliseconds-to-seconds. Each section carries the `algo`
//     identifier of the code that produced it; the server can decide
//     whether to trust the cached value or re-run the pass.
//
// Reads: callers go through ReadSidecar(path) which auto-upgrades v1/v2
// blobs to v3 in-place on disk (cheap transform; no audio touched). The
// rest of the codebase only ever sees v3.
//
// Writes: WriteSidecar(path, v3) emits the canonical v3 shape.
package library

import (
	"encoding/json"
	"fmt"
	"os"
)

// SchemaVersion is the current sidecar schema version. Bumped only on
// breaking shape changes — adding new sections to `metadata` is additive
// and doesn't bump this.
const SchemaVersion = 3

// SidecarV3 is the canonical on-disk shape post-upgrade.
type SidecarV3 struct {
	Version  int     `json:"version"` // == SchemaVersion
	Schema   string  `json:"schema"`  // "abookify-sidecar/v3"
	Language string  `json:"language,omitempty"`
	Duration float64 `json:"duration"`

	// Inputs — multi-file source mapping.
	Sources []SidecarSource `json:"sources,omitempty"`

	// Atomic outputs (expensive; cached forever).
	Words    []SidecarWord    `json:"words"`
	Silences []SidecarSilence `json:"silences,omitempty"`

	// Derived metadata sections (cheap; recomputable).
	Metadata SidecarMetadata `json:"metadata"`
}

// SidecarSource describes one file in a multi-file audiobook timeline.
// OffsetSecs is where this file's audio begins in the concatenated stream.
type SidecarSource struct {
	File         string  `json:"file"`
	OffsetSecs   float64 `json:"offset_secs"`
	DurationSecs float64 `json:"duration_secs"`
}

// SidecarWord is a single timestamped token from Whisper.
type SidecarWord struct {
	Start       float64 `json:"s"`
	End         float64 `json:"e"`
	Word        string  `json:"w"`
	Probability float64 `json:"conf,omitempty"`
}

// SidecarSilence is one acoustic-silence event from ffmpeg silencedetect
// (and/or VAD). Kind is the post-classification: chapter | paragraph |
// sentence | breath, derived from duration thresholds.
type SidecarSilence struct {
	Start    float64 `json:"s"`
	End      float64 `json:"e"`
	Duration float64 `json:"duration"`
	Source   string  `json:"source"`
	RmsDB    float64 `json:"rms_db,omitempty"`
	Kind     string  `json:"kind"`
}

// SidecarMetadata is the union of all derived sections. Each is optional —
// absent means "not yet computed." Each section carries its own algo
// identifier so the server can independently decide to recompute it.
type SidecarMetadata struct {
	Chapters   *ChapterSection   `json:"chapters,omitempty"`
	Paragraphs *ParagraphSection `json:"paragraphs,omitempty"`
	Characters *CharacterSection `json:"characters,omitempty"`
	Summaries  *SummarySection   `json:"summaries,omitempty"`
}

// SectionMeta is the common envelope for every metadata section. Algo is
// the identifier of the producer (e.g. "narrator+gap-fill@1.2"); the
// server bumps this when the algorithm output could change. Source is
// "algorithm:<id>" for computed entries, "user" for manually edited.
type SectionMeta struct {
	Algo        string `json:"algo"`
	ComputedAt  string `json:"computed_at"` // RFC3339
	SchemaNotes string `json:"schema_notes,omitempty"`
}

// ChapterSection holds the chapter boundaries (audio book level — the
// transcript split is derived from these).
type ChapterSection struct {
	SectionMeta
	Entries []SidecarChapter `json:"entries"`
}

// SidecarChapter is one chapter boundary. Kind is "chapter" | "part".
// Source distinguishes algorithm output from user edits — algorithm reruns
// must skip user-sourced entries (or, today: clobber them, per agreement).
type SidecarChapter struct {
	Index      int     `json:"index"`
	Number     int     `json:"number,omitempty"`
	Kind       string  `json:"kind,omitempty"`
	Title      string  `json:"title"`
	StartSec   float64 `json:"start_sec"`
	EndSec     float64 `json:"end_sec,omitempty"`
	WordIdx    int     `json:"word_idx,omitempty"`
	WordCount  int     `json:"word_count,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Source     string  `json:"source,omitempty"` // "algorithm:..." | "user" | "inferred-gap-fill"
}

// ParagraphSection mirrors ChapterSection but for the finer-grained
// paragraph-break boundaries derived from silence events.
type ParagraphSection struct {
	SectionMeta
	Entries []SidecarParagraph `json:"entries"`
}

// SidecarParagraph is a [start_word, end_word) span scoped to one chapter.
// ChapterIdx + ParagraphIdx are zero-based indices.
type SidecarParagraph struct {
	ChapterIdx   int `json:"chapter_idx"`
	ParagraphIdx int `json:"paragraph_idx"`
	WordStart    int `json:"word_start"`
	WordEnd      int `json:"word_end"`
}

// CharacterSection is progressive: each entry is keyed to a chapter index,
// listing characters introduced and state updates as the book progresses.
// At read time, UI takes union over chapters[0..N] for the user's current
// position N — characters appearing later are hidden (spoiler-safe).
type CharacterSection struct {
	SectionMeta
	Entries []SidecarCharacterChapter `json:"entries"`
}

// SidecarCharacterChapter holds the per-chapter character delta.
type SidecarCharacterChapter struct {
	ChapterIdx   int                       `json:"chapter_idx"`
	Introduced   []SidecarCharacterIntro   `json:"introduced,omitempty"`
	StateUpdates []SidecarCharacterUpdate  `json:"state_updates,omitempty"`
}

// SidecarCharacterIntro is a character first appearing in the chapter.
type SidecarCharacterIntro struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
}

// SidecarCharacterUpdate is a state change for a character introduced
// earlier (e.g. "Sherman: tense after the car incident").
type SidecarCharacterUpdate struct {
	Name   string `json:"name"`
	Change string `json:"change"`
}

// SummarySection is also progressive — entries[N] is the cumulative
// summary through chapter N. UI shows summaries[N] for current position N.
type SummarySection struct {
	SectionMeta
	Entries []SidecarSummary `json:"entries"`
}

// SidecarSummary is a per-chapter summary. The summary covers chapters
// [0, chapter_idx] inclusive; spoiler-safe display takes summaries[N]
// when the user is at chapter N.
type SidecarSummary struct {
	ChapterIdx int    `json:"chapter_idx"`
	Summary    string `json:"summary"`
}

// ReadSidecar reads a sidecar file from disk, auto-upgrading v1/v2 blobs
// to v3 if needed (and rewriting the file). Returns the v3 struct.
//
// On v1/v2 input: the file is rewritten as v3 in place. This is the single
// gate through which the rest of the server consumes sidecars — every
// other read path goes through here so the server only ever sees v3.
func ReadSidecar(path string) (*SidecarV3, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sidecar: %w", err)
	}

	// Peek at version field to dispatch upgrade chain.
	var probe struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(data, &probe)

	switch probe.Version {
	case 0, 1, 2:
		// v1 had no "version" field (probe.Version == 0). v2 has it set
		// to 2. Both upgrade through the same chain.
		v3, err := UpgradeToV3(data)
		if err != nil {
			return nil, fmt.Errorf("upgrade to v3: %w", err)
		}
		// Rewrite on disk so subsequent reads skip the upgrade.
		if err := WriteSidecar(path, v3); err != nil {
			// Non-fatal — we have the v3 in memory; warn but proceed.
			fmt.Fprintf(os.Stderr, "sidecar upgrade write-back failed for %s: %v\n", path, err)
		}
		return v3, nil
	case 3:
		var sc SidecarV3
		if err := json.Unmarshal(data, &sc); err != nil {
			return nil, fmt.Errorf("parse v3 sidecar: %w", err)
		}
		return &sc, nil
	default:
		return nil, fmt.Errorf("unknown sidecar version %d", probe.Version)
	}
}

// WriteSidecar emits a sidecar in canonical v3 form with stable field
// ordering and 0-padded indentation tuned for diff-friendliness.
func WriteSidecar(path string, sc *SidecarV3) error {
	sc.Version = SchemaVersion
	sc.Schema = "abookify-sidecar/v3"
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write tmp sidecar: %w", err)
	}
	// Atomic rename so a partial write never replaces the existing file.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename sidecar: %w", err)
	}
	return nil
}
