package library

import (
	"strings"
	"testing"

	"github.com/pj/abookify/internal/db"
)

// The state that matters most: a book nobody has examined must NOT render like a
// book that passed. Folding "unknown" into "fine" is how 122,759 invented words
// stayed invisible.
func TestTextTrustUncheckedIsNotVerified(t *testing.T) {
	noRow := BuildTextTrust(1, nil)
	if noRow.State != TrustUnchecked {
		t.Errorf("absent row → state %q, want %q", noRow.State, TrustUnchecked)
	}
	if strings.Contains(strings.ToLower(noRow.Headline), "matches") {
		t.Errorf("unchecked headline implies a pass: %q", noRow.Headline)
	}
	if !strings.Contains(noRow.Detail, "not the same as it being fine") {
		t.Errorf("unchecked detail must say so explicitly: %q", noRow.Detail)
	}

	// A sidecar with no confidence data is also unchecked, not clean — the
	// question could not be asked.
	noConf := BuildTextTrust(1, &db.TextTrustRow{HasConfidence: false, TotalWords: 50000})
	if noConf.State != TrustUnchecked {
		t.Errorf("no confidence data → %q, want %q", noConf.State, TrustUnchecked)
	}
}

func TestTextTrustStates(t *testing.T) {
	cases := []struct {
		suspect, total int
		want           string
	}{
		{0, 50000, TrustVerified},
		{100, 50000, TrustMinor},       // 0.2% — the library median
		{500, 50000, TrustSignificant}, // 1.0% — the boundary
		{3299, 77220, TrustSignificant},
	}
	for _, c := range cases {
		got := BuildTextTrust(1, &db.TextTrustRow{
			HasConfidence: true, SuspectWords: c.suspect, TotalWords: c.total}).State
		if got != c.want {
			t.Errorf("%d/%d → %q, want %q", c.suspect, c.total, got, c.want)
		}
	}
}

// The honest-badge principle: state the suspicion, never assert it as proof.
// Low confidence is evidence — hard audio also produces it on correct text.
func TestTextTrustCopyIsHonest(t *testing.T) {
	sig := BuildTextTrust(1, &db.TextTrustRow{HasConfidence: true, SuspectWords: 3299, TotalWords: 77220})
	if !strings.Contains(sig.Headline, "may not") {
		t.Errorf("headline overstates suspicion as fact: %q", sig.Headline)
	}
	for _, forbidden := range []string{"is wrong", "is fabricated", "invalid"} {
		if strings.Contains(strings.ToLower(sig.Detail), forbidden) {
			t.Errorf("detail asserts proof (%q): %q", forbidden, sig.Detail)
		}
	}
	// Raw counts must always be present so a reader can judge scale rather than
	// trusting the tier word.
	if !strings.Contains(sig.Detail, "3,299") || !strings.Contains(sig.Detail, "77,220") {
		t.Errorf("detail omits raw counts: %q", sig.Detail)
	}
	// And it must say playback is unaffected — the audio is fine, only the text
	// is suspect, and conflating those would read as "your audiobook is broken".
	if !strings.Contains(sig.Detail, "Playback is unaffected") {
		t.Errorf("detail does not say playback is fine: %q", sig.Detail)
	}
}

func TestCommaInt(t *testing.T) {
	for in, want := range map[int]string{0: "0", 42: "42", 999: "999",
		1000: "1,000", 77220: "77,220", 1234567: "1,234,567"} {
		if got := commaInt(in); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", in, got, want)
		}
	}
}
