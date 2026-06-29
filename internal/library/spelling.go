package library

import "strings"

// British <-> American spelling equivalence for the meld COMPARISON layer only
// (#200 sensitivity tuning). PJ's 1984 test (British ebook vs American audio
// edition) flagged orthographic variants — towards/toward, centre/center,
// litre/liter, recognised/recognized — as "Differs between sources".
// canonicalizeSpelling folds a single word to an American-canonical form so
// both spellings collapse. It is applied PER WORD inside normalizeForCompare
// and NEVER alters displayed text.
//
// Why both directions land on the same form: American spellings don't match
// the British suffixes (centre→center, but "center" ends -er not -re, so it's
// unchanged), so applying the British→American rule to both sides converges.
//
// Failure direction is deliberate: rules are return-on-first-match and
// conservatively guarded, so when in doubt we UNDER-normalize (leave a
// near-trivial diff visible) rather than over-normalize. We never want to HIDE
// a genuine word substitution — only stop flagging orthographic ones.

// britIrregular maps irregular British spellings (and a few -ise/-ice and
// -ae-/-oe- specials that don't follow a safe productive rule) to American.
var britIrregular = map[string]string{
	"grey": "gray", "greyer": "grayer", "greyest": "grayest", "greyish": "grayish",
	"tyre": "tire", "tyres": "tires",
	"mould": "mold", "moulds": "molds", "moulded": "molded", "moulding": "molding", "mouldy": "moldy",
	"plough": "plow", "ploughs": "plows", "ploughed": "plowed", "ploughing": "plowing",
	"kerb": "curb", "kerbs": "curbs",
	"programme": "program", "programmes": "programs",
	"cheque": "check", "cheques": "checks",
	"storey": "story", "storeys": "stories",
	"aluminium": "aluminum",
	"pyjamas": "pajamas",
	"sceptic": "skeptic", "sceptics": "skeptics", "sceptical": "skeptical", "scepticism": "skepticism",
	"moustache": "mustache", "moustaches": "mustaches", "moustached": "mustached",
	"draught": "draft", "draughts": "drafts", "draughty": "drafty",
	"cosy": "cozy", "cosier": "cozier", "cosiest": "coziest", "cosily": "cozily",
	"smoulder": "smolder", "smouldered": "smoldered", "smouldering": "smoldering",
	"woollen": "woolen", "woollens": "woolens", "gaol": "jail", "gaoler": "jailer",
	"practise": "practice", "practised": "practiced", "practises": "practices", "practising": "practicing",
	// -ae-/-oe-: a blanket ae/oe→e rule wrecks poem/shoe/toe/does/canoe, so the
	// real variants are enumerated rather than rule-derived.
	"encyclopaedia": "encyclopedia", "encyclopaedias": "encyclopedias",
	"foetus": "fetus", "foetuses": "fetuses", "foetal": "fetal",
	"paediatric": "pediatric", "paediatrics": "pediatrics", "paediatrician": "pediatrician",
	"anaemia": "anemia", "anaemic": "anemic",
	"oestrogen": "estrogen", "diarrhoea": "diarrhea", "coeliac": "celiac",
	"archaeology": "archeology", "archaeological": "archeological",
	"gynaecology": "gynecology", "haemorrhage": "hemorrhage", "haemoglobin": "hemoglobin",
	"leukaemia": "leukemia", "oesophagus": "esophagus",
	"orthopaedic": "orthopedic", "faeces": "feces",
	"anaesthesia": "anesthesia", "anaesthetic": "anesthetic",
	"amoeba": "ameba", "manoeuvre": "maneuver", "manoeuvres": "maneuvers", "manoeuvred": "maneuvered",
	"aesthetic": "esthetic", "aesthetics": "esthetics", "mediaeval": "medieval",
}

// ourExceptions: -our STEMS that are NOT British variants and whose -or form
// (incl. -ed/-ing/-er derivatives) would collide with a different real word
// (pour→pore: pouring/poring, scour→score: scouring/scoring) or be nonsense.
// The lookup keys off the stem (w[:idx+3]), so one entry covers every
// inflection (pour/pours/poured/pouring/pourer). Short words (four/hour/your)
// are handled by length guards.
var ourExceptions = map[string]bool{
	"flour": true, "scour": true, "velour": true, "detour": true,
	"contour": true, "devour": true, "pour": true, "sour": true,
	"tour": true, "dour": true,
}

// reExceptions: words ending in -re that aren't variants, where the -re→-er
// transform would either be nonsense or collide with a different word
// (timbre→timber). ochre/sabre ARE variants, so they're deliberately absent.
var reExceptions = map[string]bool{
	"genre": true, "genres": true, "acre": true, "acres": true, "ogre": true, "ogres": true,
	"massacre": true, "massacres": true, "mediocre": true, "lucre": true, "nacre": true,
	"macabre": true, "cadre": true, "cadres": true, "euchre": true,
	"timbre": true, "timbres": true, "oeuvre": true, "oeuvres": true,
}

// wardsExceptions: -wards words that aren't directional adverbs (reward+s etc.).
var wardsExceptions = map[string]bool{
	"rewards": true, "awards": true, "cowards": true,
}

func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// canonicalizeSpelling folds a lowercased, alphanumeric word to its American
// canonical form for comparison. Returns the input unchanged when no rule
// applies.
func canonicalizeSpelling(w string) string {
	if len(w) < 4 {
		return w
	}
	if v, ok := britIrregular[w]; ok {
		return v
	}
	has := func(suf string) bool { return strings.HasSuffix(w, suf) }
	cut := func(suf, repl string) string { return w[:len(w)-len(suf)] + repl }

	// -wards → -ward (towards→toward), directional adverbs only.
	if has("wards") && len(w) >= 6 && !wardsExceptions[w] {
		return cut("wards", "ward")
	}
	// -ise/-ize family (recognise/recognised/organisation → -ize).
	switch {
	case has("isations"):
		return cut("isations", "izations")
	case has("isation"):
		return cut("isation", "ization")
	case has("ising") && len(w) >= 6:
		return cut("ising", "izing")
	case has("ised") && len(w) >= 5:
		return cut("ised", "ized")
	case has("isers") && len(w) >= 6:
		return cut("isers", "izers")
	case has("ises") && len(w) >= 5:
		return cut("ises", "izes")
	case has("iser") && len(w) >= 5:
		return cut("iser", "izer")
	case has("ise") && len(w) >= 5:
		return cut("ise", "ize")
	}
	// -yse → -yze (analyse→analyze, paralyse→paralyze).
	switch {
	case has("ysing"):
		return cut("ysing", "yzing")
	case has("ysed"):
		return cut("ysed", "yzed")
	case has("yses"):
		return cut("yses", "yzes")
	case has("yse"):
		return cut("yse", "yze")
	}
	// -our → -or, word-final (colour→color, honour→honor) or before a British
	// derivational suffix (coloured→colored, favourite→favorite, colourful→
	// colorful, neighbourhood→neighborhood, labourer→laborer). The colliding
	// stems (pour/scour/sour/tour…) are in ourExceptions so pouring/poring,
	// scouring/scoring stay distinct; short words are length-guarded.
	if i := strings.LastIndex(w, "our"); i >= 1 {
		if base := w[:i+3]; !ourExceptions[base] {
			switch rest := w[i+3:]; rest {
			case "": // word-final: colour→color
				if len(w) >= 5 {
					return w[:i] + "or"
				}
			case "s": // plural: colours→colors
				if len(w) >= 6 {
					return w[:i] + "ors"
				}
			case "ed", "ing", "er", "ers", "ite", "ites", "able", "ables",
				"ably", "ful", "fully", "hood", "less", "ism", "ous", "y":
				return w[:i] + "or" + rest
			}
		}
	}
	// -re → -er (centre→center, litre→liter, theatre→theater), only when a
	// consonant precedes "re" (skips score/store/more/genre…).
	if has("re") && len(w) >= 4 && !reExceptions[w] && !isVowelByte(w[len(w)-3]) {
		return cut("re", "er")
	}
	// -ogue(s) → -og(s) (catalogue→catalog, dialogue→dialog).
	if has("ogues") {
		return cut("ogues", "ogs")
	}
	if has("ogue") {
		return cut("ogue", "og")
	}
	// -ence(s) → -ense(s) (defence→defense, licence→license).
	if has("ences") && len(w) >= 6 {
		return cut("ences", "enses")
	}
	if has("ence") && len(w) >= 5 {
		return cut("ence", "ense")
	}
	// Doubled-l before a suffix → single l (travelled→traveled, labelled→
	// labeled, modelling→modeling, marvellous→marvelous, traveller→traveler).
	switch {
	case has("lling") && len(w) >= 6:
		return cut("lling", "ling")
	case has("lled") && len(w) >= 5:
		return cut("lled", "led")
	case has("llors"):
		return cut("llors", "lors")
	case has("llor"):
		return cut("llor", "lor")
	case has("llers") && len(w) >= 6:
		return cut("llers", "lers")
	case has("ller") && len(w) >= 5:
		return cut("ller", "ler")
	case has("llous"):
		return cut("llous", "lous")
	}
	return w
}
