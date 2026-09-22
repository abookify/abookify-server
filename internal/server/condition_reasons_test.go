package server

import (
	"strings"
	"testing"
)

func TestHumanConditionReason(t *testing.T) {
	got := humanConditionReason("sidecar integrity: synthesized_word_timings, model_did_not_believe_this")
	if strings.Contains(got, "_") || strings.Contains(got, "sidecar") {
		t.Errorf("a field name leaked into the sentence: %q", got)
	}
	if !strings.Contains(got, "highlighting may drift") || !strings.Contains(got, "wasn't confident") {
		t.Errorf("both reasons must be explained: %q", got)
	}
	if u := humanConditionReason("sidecar integrity: some_new_guard"); !strings.Contains(u, "some_new_guard") || !strings.HasPrefix(u, "Something went wrong") {
		t.Errorf("unknown codes must still be shown, in a sentence: %q", u)
	}
	if humanConditionReason("") != "" {
		t.Errorf("empty stays empty")
	}
}
