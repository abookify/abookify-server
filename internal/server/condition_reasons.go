package server

import "strings"

// Condition reasons arrive as the producer's codes ("sidecar integrity:
// synthesized_word_timings, model_did_not_believe_this"). Those are the right
// thing to STORE — they say exactly which guard fired — and the wrong thing to
// SHOW: a field name where a sentence belongs (a Selfish Gene card read "⚠
// Needs review — sidecar integrity: synthesized_word_timings"). The server
// owns the words, per PJ's rule: name what it means for the reader, not what
// the system calls it. Web and mobile render `condition_reason` verbatim;
// `condition_reason_code` keeps the raw code for anyone who needs it.
var conditionReasonWords = map[string]string{
	"synthesized_word_timings":   "Some word timings in this narration were estimated rather than heard, so the highlighting may drift in places.",
	"model_did_not_believe_this": "The transcriber wasn't confident about parts of this narration, so some words in the text may be wrong or missing.",
	"truncated_audio":            "Part of this narration is missing — the audio ends before the text does.",
	"short_transcript":           "The transcript covers much less than the narration, so read-along will stop early.",
}

// humanConditionReason turns a producer's reason code(s) into sentences a
// person can act on. Unknown codes are not hidden: they are shown as a
// sentence with the code in it, so nothing silently reads as fine.
func humanConditionReason(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	body := code
	if i := strings.Index(body, ":"); i >= 0 && strings.HasPrefix(strings.ToLower(body), "sidecar integrity") {
		body = body[i+1:]
	}
	var out []string
	for _, tok := range strings.Split(body, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if w, ok := conditionReasonWords[tok]; ok {
			out = append(out, w)
		} else {
			out = append(out, "Something went wrong while this part was produced ("+tok+").")
		}
	}
	return strings.Join(out, " ")
}
