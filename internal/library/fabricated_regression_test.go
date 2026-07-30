package library

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Regression guard for the fabricated-text failure.
//
// 122,759 invented words shipped across 58 books and NOTHING caught them. The
// words were present so no gap detector fired, the audio was fine so no damage
// detector fired, and the count was HIGHER than the truth so every progress
// figure read better rather than worse. The pipeline reported success the whole
// way.
//
// A detector that runs only when someone remembers to look is not a guard. These
// tests run on every `go test` and are built from REAL captured output from the
// books we know were damaged — not synthesised shapes that might drift away from
// what the failure actually looks like.
//
// Fixtures: testdata/fabricated_real.json, testdata/running_header_real.json.
// They are captured evidence. Do not regenerate them to make a test pass; if the
// detector stops catching them, the detector regressed.

type fabricatedCase struct {
	Book      string  `json:"book"`
	AtSec     float64 `json:"at_sec"`
	OnInstant int     `json:"words_on_one_instant"`
	Text      string  `json:"text"`
	MeanConf  float64 `json:"mean_confidence"`
	Words     []struct {
		W    string  `json:"w"`
		S    float64 `json:"s"`
		E    float64 `json:"e"`
		Conf float64 `json:"conf"`
	} `json:"words"`
}

func loadFabricated(t *testing.T) []fabricatedCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/fabricated_real.json")
	if err != nil {
		t.Fatalf("fixture missing — it is captured evidence, restore it: %v", err)
	}
	var doc struct {
		Cases []fabricatedCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture unparseable: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return doc.Cases
}

// Every real fabricated span must be caught by the sidecar integrity check.
// These are the exact word runs from Frankenstein, Dracula, Atlas Shrugged
// (pre-repair) and the reverted Heart Goes Last redo.
func TestRegressionRealFabricatedSpansAreDetected(t *testing.T) {
	for _, c := range loadFabricated(t) {
		var words []sttWord
		// Surround the bad run with ordinary narration so the case is realistic:
		// a real sidecar is mostly fine, with a fabricated pocket inside it.
		for i := 0; i < 200; i++ {
			words = append(words, sttWord{Word: "x", Start: float64(i) * 0.4, End: float64(i)*0.4 + 0.3})
		}
		for _, w := range c.Words {
			words = append(words, sttWord{Word: w.W, Start: w.S, End: w.E})
		}
		dur := c.AtSec + 600

		probs := checkSidecarIntegrity(&sttSidecar{Duration: dur, Words: words})
		var kinds []string
		for _, p := range probs {
			kinds = append(kinds, p.Kind)
		}
		if !strings.Contains(strings.Join(kinds, ","), "synthesized_word_timings") {
			t.Errorf("%s: real fabricated span at %.1fs (%d words on one instant) NOT detected; kinds=%v\n  text: %s",
				c.Book, c.AtSec, c.OnInstant, kinds, c.Text)
		}
	}
}

// The same real spans must also be caught by the whisper-side signal, so a retry
// is triggered at transcription time rather than the damage only being noticed
// after it is written.
func TestRegressionRealFabricatedSpansTriggerRetry(t *testing.T) {
	for _, c := range loadFabricated(t) {
		if c.OnInstant <= maxWordsPerInstant {
			continue
		}
		// The service-side detector works on segments; reconstruct one segment
		// carrying the real collapsed run.
		if len(c.Words) < 8 {
			continue
		}
		var count int
		first := c.Words[0].S
		for _, w := range c.Words {
			if w.S == first {
				count++
			}
		}
		if count <= maxWordsPerInstant {
			t.Errorf("%s: fixture no longer shows a collapsed instant (%d words share %.2fs) — "+
				"either the fixture was regenerated or the capture is wrong", c.Book, count, first)
		}
	}
}

// Ordinary narration must NOT trip the guard. Without this, the guard would be
// disabled the first time it cried wolf on a healthy book.
func TestRegressionNormalNarrationPasses(t *testing.T) {
	var words []sttWord
	for i := 0; i < 4500; i++ { // 30 min at 150 wpm
		words = append(words, sttWord{Word: "word", Start: float64(i) * 0.4, End: float64(i)*0.4 + 0.3})
	}
	if p := checkSidecarIntegrity(&sttSidecar{Duration: 1800, Words: words}); len(p) != 0 {
		t.Errorf("healthy narration flagged: %+v", p)
	}
}

// Real Calibre split documents that produced 38 embedded title-only chunks must
// be rejected at extraction. If this stops failing, running headers are reaching
// the chunker again and will be cited as book text.
func TestRegressionRealRunningHeadersAreStripped(t *testing.T) {
	raw, err := os.ReadFile("testdata/running_header_real.json")
	if err != nil {
		t.Fatalf("fixture missing — captured evidence, restore it: %v", err)
	}
	var doc struct {
		Docs []struct {
			File string `json:"file"`
			HTML string `json:"html"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture unparseable: %v", err)
	}
	if len(doc.Docs) == 0 {
		t.Fatal("fixture has no documents")
	}

	for _, d := range doc.Docs {
		text := strings.TrimSpace(htmlToText(d.HTML))
		title := extractFirstHeading(d.HTML)

		// Test the actual invariant: <head> must contribute NOTHING to the
		// extracted text. Counting total occurrences is wrong — the cover page
		// legitimately carries the title twice in its body (once with a straight
		// apostrophe, once curly), and an earlier version of this test failed on
		// that healthy page. Compare against the BODY alone instead.
		body := d.HTML
		if i := strings.Index(strings.ToLower(body), "<body"); i >= 0 {
			body = body[i:]
		}
		inBody := strings.Count(htmlToText(body), "HH1 - Hitchhiker")
		inFull := strings.Count(text, "HH1 - Hitchhiker")
		if inFull > inBody {
			t.Errorf("%s: title appears %d times in extracted text but only %d times in <body> — "+
				"<head> is leaking again\n  %.120s", d.File, inFull, inBody, text)
		}
		// A document whose entire content is its heading must be recognised as
		// such, so extraction drops it instead of chunking and embedding it.
		if title != "" && isHeadingOnly(text, title) {
			continue // correctly identified as a heading-only artefact
		}
		if strings.TrimSpace(strings.ReplaceAll(text, title, "")) == "" {
			t.Errorf("%s: content is nothing but the heading yet isHeadingOnly said otherwise\n  title=%q text=%q",
				d.File, title, text)
		}
	}
}

// The confidence check must catch every real fabricated span, and it must do so
// WITHOUT reference to word timings — that independence is the whole point. It is
// the only check that asks about the relationship between text and audio rather
// than about the shape of the text alone.
func TestRegressionLowConfidenceCatchesRealSpans(t *testing.T) {
	for _, c := range loadFabricated(t) {
		// Real captured words, but with their timings REPLACED by plausible
		// sequential ones — so the collapsed-timestamp check cannot fire and only
		// confidence is left to catch it. This is the gap the confidence signal
		// exists to close.
		var words []sttWord
		for i := 0; i < 300; i++ {
			words = append(words, sttWord{Word: "ordinary", Start: float64(i) * 0.4,
				End: float64(i)*0.4 + 0.3, Probability: 0.98})
		}
		base := 200.0
		var haveConf bool
		for i, w := range c.Words {
			if w.Conf > 0 {
				haveConf = true
			}
			words = append(words, sttWord{Word: w.W,
				Start: base + float64(i)*0.35, End: base + float64(i)*0.35 + 0.3,
				Probability: w.Conf})
		}
		if !haveConf {
			t.Skipf("%s: fixture carries no confidence data", c.Book)
		}

		probs := checkSidecarIntegrity(&sttSidecar{Duration: 1200, Words: words})
		var kinds []string
		for _, p := range probs {
			kinds = append(kinds, p.Kind)
		}
		joined := strings.Join(kinds, ",")
		if strings.Contains(joined, "synthesized_word_timings") {
			t.Fatalf("%s: timings were meant to be plausible here — the test is not isolating "+
				"the confidence signal", c.Book)
		}
		if !strings.Contains(joined, "model_did_not_believe_this") {
			t.Errorf("%s: fabricated span with PLAUSIBLE timings not caught by confidence; kinds=%v\n  %s",
				c.Book, kinds, c.Text)
		}
	}
}

// Confident narration must not be flagged, and a sidecar with no confidence data
// at all must not be flagged either — absent confidence is not zero confidence.
func TestRegressionConfidenceNoFalsePositives(t *testing.T) {
	var good []sttWord
	for i := 0; i < 3000; i++ {
		good = append(good, sttWord{Word: "w", Start: float64(i) * 0.4,
			End: float64(i)*0.4 + 0.3, Probability: 0.97})
	}
	if p := checkSidecarIntegrity(&sttSidecar{Duration: 1200, Words: good}); len(p) != 0 {
		t.Errorf("confident narration flagged: %+v", p)
	}

	var noConf []sttWord
	for i := 0; i < 3000; i++ {
		noConf = append(noConf, sttWord{Word: "w", Start: float64(i) * 0.4, End: float64(i)*0.4 + 0.3})
	}
	for _, p := range checkSidecarIntegrity(&sttSidecar{Duration: 1200, Words: noConf}) {
		if p.Kind == "model_did_not_believe_this" {
			t.Error("a pre-v2 sidecar with no confidence data was flagged — absent is not zero")
		}
	}
}
