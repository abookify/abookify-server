package server

import "net/http"

// Shared gap-status presentation contract. The transcription `cause` field is
// classified by transcription; how it PRESENTS (severity level, label, whether a
// retry is safe) is a cross-lane decision that web AND mobile must render
// identically — otherwise the same book says two things and PJ trusts neither.
//
// This is that contract, served from ONE place (mirrors the #202 settings
// schema): both clients fetch it and map `level` to their own colour token, so
// the cause→level→action mapping changes in exactly one file. When transcription
// tightens the spec again (it already has once — dropped_segment moved out of
// "missing" because the audio plays fine), only GapStatusModelDoc changes and
// both surfaces follow by construction, not by anyone remembering.
//
// Contract for clients:
//   - `level` is the severity token; map it to a platform colour (missing=red,
//     unverified=amber, text_incomplete=neutral). Do NOT hardcode per-cause colour.
//   - `label` may contain the literal "{duration}"; substitute the humanised
//     total_missing_sec. Causes with show_without_gap render even when
//     segment_count == 0 (damaged_source) and their label carries no {duration}.
//   - `retryable` gates the one-click Retry — NEVER offer retry when false
//     (a wrong retry spends an hour of GPU for nothing). Absent/unknown cause
//     ⇒ use the "unknown" entry (neutral, not retryable).

// GapStatusModelVersion bumps on a breaking shape change (new level token,
// renamed field). Additive cause/field changes keep the version.
const GapStatusModelVersion = 1

// GapCausePresentation is how one `cause` renders.
type GapCausePresentation struct {
	Level          string `json:"level"`            // missing | unverified | text_incomplete
	Label          string `json:"label"`            // may contain "{duration}"
	Detail         string `json:"detail"`           // one-line explanation
	Action         string `json:"action"`           // retry | reacquire | scan | none
	Retryable      bool   `json:"retryable"`        // gate the one-click Retry
	ShowWithoutGap bool   `json:"show_without_gap"` // render even with segment_count == 0
}

// GapStatusModel is the GET /api/transcription-gaps/legend payload.
type GapStatusModel struct {
	Version int                             `json:"version"`
	Levels  []string                        `json:"levels"` // severity high→low
	Causes  map[string]GapCausePresentation `json:"causes"`
}

// GapStatusModelDoc encodes transcription's TIGHTENED four-state model. The split
// is by WHICH artefact is damaged: truncated/damaged affect the AUDIO (the book
// is genuinely short), dropped_segment affects only the TEXT (audio plays fine —
// so it must NOT read as "missing from the book").
func GapStatusModelDoc() GapStatusModel {
	return GapStatusModel{
		Version: GapStatusModelVersion,
		Levels:  []string{"missing", "unverified", "text_incomplete"},
		Causes: map[string]GapCausePresentation{
			// AUDIO missing — the recording itself is short. Re-acquire; no retry.
			"truncated_source": {
				Level:     "missing",
				Label:     "{duration} missing from the recording",
				Detail:    "This audio was never fully downloaded — re-acquire a complete copy of the recording. Re-transcribing cannot recover it.",
				Action:    "reacquire",
				Retryable: false,
			},
			// AUDIO unreadable past a point — we can't vouch it's complete. Scan.
			"damaged_source": {
				Level:          "unverified",
				Label:          "Recording may be incomplete",
				Detail:         "The audio file has decode errors past a point, so part of the recording may be unreadable. Scan the source for details.",
				Action:         "scan",
				Retryable:      false,
				ShowWithoutGap: true,
			},
			// Only the TRANSCRIPT is incomplete — audio plays fine. Re-run STT.
			"dropped_segment": {
				Level:     "text_incomplete",
				Label:     "{duration} not transcribed",
				Detail:    "Audio plays normally; only the text, search and Q&A are incomplete across this span. Re-running transcription recovers it.",
				Action:    "retry",
				Retryable: true,
			},
			// Not transcribed, cause not classified (unscanned / older server).
			// Neutral, and NEVER retryable — never guess a retry is safe.
			"unknown": {
				Level:     "text_incomplete",
				Label:     "{duration} not transcribed",
				Detail:    "Part of this audio has no transcript, and the source hasn't been scanned to classify why.",
				Action:    "none",
				Retryable: false,
			},
		},
	}
}

// handleGapStatusLegend serves the shared gap-status presentation contract.
// Static + cacheable; the SAME payload web + mobile render from.
func (s *Server) handleGapStatusLegend(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, GapStatusModelDoc())
}
