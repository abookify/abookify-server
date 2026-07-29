package server

import "net/http"

// Per-provider option lists for the STT/TTS lanes, mirroring /api/llm/models so
// all three per-feature model dropdowns render from one dynamic mechanism rather
// than three parallel ones. Shape is uniform: {provider: [{id,label,group?}]},
// where an empty-string provider key is the local engine. An optional group lets
// the client build <optgroup>s (used by the voice catalog); llm/stt items omit
// it and render flat.

type modelChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
}

// handleListSTTModels returns transcription model choices per provider. Curated
// and CORRECTNESS-GATED: OpenAI STT offers ONLY whisper-1 — the model proven
// (internal/stt live fidelity test) to return the word-level timestamps the
// anchor aligner + karaoke consume. gpt-4o-transcribe's word-timestamp fidelity
// is unverified, so it is deliberately not offered: a choice that silently broke
// karaoke would be worse than no choice. Breadth is not the goal.
func (s *Server) handleListSTTModels(w http.ResponseWriter, r *http.Request) {
	out := map[string][]modelChoice{
		"": {
			{ID: "large-v3", Label: "Large V3 — best quality, slowest"},
			{ID: "medium", Label: "Medium — good quality, moderate speed"},
			{ID: "small", Label: "Small — decent quality, fast"},
			{ID: "base", Label: "Base — basic quality, fastest"},
		},
		"openai": {
			{ID: "whisper-1", Label: "Whisper v2 — word timestamps verified"},
		},
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListTTSVoices returns narration voice choices per provider, grouped.
// Local Kokoro exposes its full catalog (reusing the schema's kokoroVoiceGroups
// so the two never drift); OpenAI exposes its six voices, with the default tts-1
// model chosen for it in the client — breadth is not the goal, correctness is.
func (s *Server) handleListTTSVoices(w http.ResponseWriter, r *http.Request) {
	out := map[string][]modelChoice{}
	var local []modelChoice
	for _, g := range kokoroVoiceGroups {
		for _, o := range g.Options {
			local = append(local, modelChoice{ID: o.Value, Label: o.Label, Group: g.Label})
		}
	}
	out[""] = local
	out["openai"] = []modelChoice{
		{ID: "alloy", Label: "Alloy", Group: "OpenAI voices"},
		{ID: "echo", Label: "Echo", Group: "OpenAI voices"},
		{ID: "fable", Label: "Fable", Group: "OpenAI voices"},
		{ID: "onyx", Label: "Onyx", Group: "OpenAI voices"},
		{ID: "nova", Label: "Nova (default)", Group: "OpenAI voices"},
		{ID: "shimmer", Label: "Shimmer", Group: "OpenAI voices"},
	}
	writeJSON(w, http.StatusOK, out)
}
