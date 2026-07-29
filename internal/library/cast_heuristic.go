// cast_heuristic.go — lightweight, no-container cast-of-characters extraction.
// The DEFAULT "instant cast" path (BookNLP is the optional "deep" upgrade).
//
// Signal: a token that appears Title-Case MID-sentence (not just after a period)
// is almost certainly a proper noun — this separates names from sentence-initial
// common words AND catches names that are themselves dictionary words (Lucy,
// Victor, Rose). Frequency of those mid-sentence caps ≈ character importance.
// The English dictionary is a secondary boost (out-of-dictionary → slightly
// higher), not the gate. Front-matter/boilerplate and a small place gazetteer
// are filtered/flagged. Pure text statistics — no model, no GPU, <1s per book.
package library

import (
	_ "embed"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/pj/abookify/internal/db"
)

//go:embed data/english_words.txt
var englishWordsRaw string

var (
	castDict     map[string]struct{}
	castDictOnce sync.Once
)

func castDictionary() map[string]struct{} {
	castDictOnce.Do(func() {
		castDict = make(map[string]struct{}, 80000)
		for _, w := range strings.Split(englishWordsRaw, "\n") {
			if w != "" {
				castDict[w] = struct{}{}
			}
		}
	})
	return castDict
}

// CastMember is one detected character candidate (ranked by Mentions).
type CastMember struct {
	Name     string `json:"name"`
	Mentions int    `json:"mentions"` // title-case mid-sentence count (≈ importance)
	Total    int    `json:"total"`
	InDict   bool   `json:"in_dictionary"`
	IsPlace  bool   `json:"is_place"` // matched the place gazetteer (likely a location)
}

var (
	castStop   = words2set(`the a an and or but if then of to in on at by for with from as is was were be been being have has had do does did will would shall should may might must can could i you he she it we they me him her us them my your his its our their this that these those there here what which who whom whose when where why how not no nor so than too very just now then once mr mrs miss dr sir lady lord god yes oh ah`)
	castBoiler = words2set(`gutenberg project foundation literary archive ebook etext donations trademark copyright pglaf hart michael online www http paragraph chapter chapters stave staves volume canto cantos prologue epilogue preface contents illustrations appendix section part`)
	castPlaces = words2set(`london england geneva paris france germany europe america scotland ireland italy rome switzerland turkey russia india china japan africa asia thames danube rhine alps transylvania whitby varna carfax ingolstadt mont blanc states united kingdom british english french german american russian arabian turk oriental east west north south`)
	// Titles whose trailing period ABBREVIATES rather than ends a sentence.
	// Without this the dot in "Mr. Kirwin" reads as a full stop, so Kirwin looks
	// sentence-initial and never earns a mid-sentence mention — the whole basis
	// of the ranking. It cost Frankenstein three real characters (Mr. Kirwin,
	// M. Waldman, M. Krempe), all of them people who are essentially ALWAYS
	// introduced by title, so they scored ~0 rather than merely low.
	castHonorific = words2set(`mr mrs ms miss dr m mme mlle prof professor capt col rev`)
	// A word (letters + internal apostrophe/hyphen) OR a run of sentence-ending
	// punctuation. \p{L}, not [A-Za-z]: an ASCII-only class SEVERS a name at its
	// first accented letter, so Garnett's "Svidrigaïlov" (207 mentions, a lead in
	// Crime and Punishment) tokenized as "Svidriga" — a mangled name in the cast,
	// and its mentions split across the fragment. Same for any transliterated,
	// French or German name.
	castTokenRe = regexp.MustCompile(`\p{L}[\p{L}'\-]*|[.!?]+`)
)

func words2set(s string) map[string]struct{} {
	m := make(map[string]struct{})
	for _, w := range strings.Fields(s) {
		m[w] = struct{}{}
	}
	return m
}

// ExtractCastHeuristic returns character candidates ranked by mid-sentence
// title-case frequency, with tightly-bound multi-word names merged (e.g.
// "Van Helsing", "Sherlock Holmes", "De Lacey"). minMentions is the recurrence
// floor (3 is a good default).
func ExtractCastHeuristic(chapters []db.Chapter, minMentions int) []CastMember {
	if minMentions < 1 {
		minMentions = 3
	}
	dict := castDictionary()

	var sb strings.Builder
	for _, ch := range chapters {
		t := strings.ToLower(ch.Title)
		if strings.Contains(t, "gutenberg") || strings.Contains(t, "license") {
			continue // skip PG front-matter / license chapters
		}
		sb.WriteString(ch.Content)
		sb.WriteByte('\n')
	}
	text := sb.String()

	midcap := map[string]int{}   // Title-case mid-sentence (strong name signal)
	titlecap := map[string]int{} // Title-case anywhere (mid + sentence-initial)
	total := map[string]int{}
	allcaps := map[string]int{}
	display := map[string]string{}
	bigram := map[string]int{} // "a b" (adjacent Title-case tokens)
	bigramDisp := map[string]string{}

	var prevLow, prevSurf string
	var prevTitle bool
	prevEnd := true
	honorific := "" // surface form of a title awaiting its name ("Mr", "M")
	for _, tok := range castTokenRe.FindAllString(text, -1) {
		if c := tok[0]; c == '.' || c == '!' || c == '?' {
			// A single dot right after a title abbreviates it — leave prevEnd
			// alone so the name that follows still counts as mid-sentence.
			if honorific != "" && tok == "." {
				continue
			}
			prevEnd, prevTitle = true, false
			honorific = ""
			continue
		}
		low := strings.TrimSuffix(strings.ToLower(tok), "'s")
		total[low]++
		isTitle := false
		switch {
		case isAllCapsWord(tok):
			allcaps[low]++
		case unicode.IsUpper([]rune(tok)[0]):
			isTitle = true
			titlecap[low]++
			if !prevEnd {
				midcap[low]++
			}
			if display[low] == "" {
				// Carry the title into the display name ("Mr. Kirwin") — it is
				// how the book refers to them, and it separates the servant
				// "Mr. Kirwin" from a bare surname used for someone else.
				if honorific != "" {
					display[low] = honorific + ". " + tok
				} else {
					display[low] = tok
				}
			}
		}
		if _, isHon := castHonorific[low]; isHon {
			honorific = tok
			prevLow, prevSurf, prevTitle, prevEnd = low, tok, isTitle, false
			continue
		}
		honorific = ""
		if isTitle && prevTitle && !isCastNoise(prevLow) && !isCastNoise(low) {
			key := prevLow + " " + low
			bigram[key]++
			if bigramDisp[key] == "" {
				bigramDisp[key] = prevSurf + " " + tok
			}
		}
		prevLow, prevSurf, prevTitle, prevEnd = low, tok, isTitle, false
	}

	// Merge a bigram "A B" into one entity ONLY when both components appear
	// MOSTLY together (each ≥50% inside the bigram) — that's a real multi-word
	// name (Van Helsing), vs a loose co-occurrence (Mina Harker) which stays split.
	consumed := map[string]bool{}
	var out []CastMember
	for key, bc := range bigram {
		if bc < minMentions {
			continue
		}
		parts := strings.SplitN(key, " ", 2)
		a, b := parts[0], parts[1]
		if titlecap[a] == 0 || titlecap[b] == 0 {
			continue
		}
		if float64(bc) >= 0.5*float64(titlecap[a]) && float64(bc) >= 0.5*float64(titlecap[b]) {
			consumed[a], consumed[b] = true, true
			mentions := bc
			if midcap[a] > mentions {
				mentions = midcap[a]
			}
			if midcap[b] > mentions {
				mentions = midcap[b]
			}
			_, placeA := castPlaces[a]
			_, placeB := castPlaces[b]
			out = append(out, CastMember{
				Name: bigramDisp[key], Mentions: mentions, Total: total[a] + total[b],
				InDict: false, IsPlace: placeA || placeB,
			})
		}
	}

	for w, mc := range midcap {
		if consumed[w] || len(w) < 2 || mc < minMentions || isCastNoise(w) {
			continue
		}
		if allcaps[w]*2 > total[w] {
			continue // mostly ALL-CAPS → heading / SHOUTING, not a name
		}
		_, inDict := dict[w]
		_, isPlace := castPlaces[w]
		name := display[w]
		if name == "" {
			name = strings.ToUpper(w[:1]) + w[1:]
		}
		out = append(out, CastMember{Name: name, Mentions: mc, Total: total[w], InDict: inDict, IsPlace: isPlace})
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := castScore(out[i]), castScore(out[j])
		if si != sj {
			return si > sj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func isCastNoise(w string) bool {
	// A title is never a character, and never half of a multi-word name: without
	// this "M" pairs with the following surname into a bigram entity "M Waldman",
	// beating the honorific path that would have produced "M. Waldman".
	// (castStop already covers mr/mrs/miss/dr, but not m/ms/mme/mlle/prof/capt.)
	if _, ok := castHonorific[w]; ok {
		return true
	}
	if _, ok := castStop[w]; ok {
		return true
	}
	_, ok := castBoiler[w]
	return ok
}

func castScore(c CastMember) float64 {
	s := float64(c.Mentions)
	if !c.InDict {
		s *= 1.4 // out-of-dictionary → more likely a proper name
	}
	if c.IsPlace {
		s *= 0.02 // places sink to the bottom (flagged, not dropped)
	}
	return s
}

func isAllCapsWord(s string) bool {
	if len(s) < 2 {
		return false
	}
	hasAlpha := false
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsUpper(r) {
			hasAlpha = true
		}
	}
	return hasAlpha
}
