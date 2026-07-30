// Answering "do the database rows still say what the sidecar on disk says?"
//
// This exists because the answer was NO and nothing could tell us. stt-cli writes
// a sidecar file; the reader, search and Q&A all read the database. A repaired or
// re-transcribed sidecar therefore improves nothing until it is imported, and an
// un-imported one is INVISIBLE — the work has a transcript, chapters, chunks and
// embeddings, all fully populated, all describing a decode that was thrown away.
//
// Atlas Shrugged sat in exactly that state for hours after its repair: the reader
// and Q&A served "ojo ajue 익 jo ya lasgunos afenаз" while the corrected words lay
// in a file on disk. No detector fired, because every detector was asking whether
// the DATA was well-formed, and it was — it was well-formed and stale.
//
// The check is deliberately content-based, not timestamps or counts. mtimes lie
// (a copy, a restore, a syncthing round-trip), and 438 Days re-transcribed to
// within 2.2% of its old length, so any count tolerance would have called it
// identical.
//
// It is also EXHAUSTIVE rather than sampled, and that mattered: the first version
// sampled twelve phrases and cleared Atlas Shrugged while Atlas was the known-stale
// case it was written to catch. Only 2.5% of that book's words were fabricated, so
// twelve probes landed in the 97.5% that both decodes agree on. Comparing every
// n-gram costs one pass and one hash set — a rounding error against a 550,000-word
// book — and answers the question instead of estimating it.
//
// Thresholds are calibrated against labelled real books (see sidecar_drift_test.go
// and the handoff), not chosen by feel.
package library

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// Drift verdicts.
const (
	DriftOK          = "ok"            // the database reflects the sidecar
	DriftStale       = "stale"         // sidecar on disk differs from the imported text
	DriftNotImported = "not_imported"  // sidecar exists, no transcript book at all
	DriftNoSidecar   = "no_sidecar"    // nothing to compare (never transcribed)
	DriftUnreadable  = "unreadable"    // sidecar present but unparseable
	DriftChunksOnly  = "chunks_stale"  // text matches, retrieval index does not
)

// driftGram is the n-gram width compared. Long enough that a match is not
// coincidence, short enough that the import's own edits (it strips narrator title
// announcements, re-splits paragraphs) only break the few grams that straddle them.
const driftGram = 8

// driftMinMatchPct is the share of the sidecar's n-grams that must appear in the
// stored transcript before the database is called current.
//
// CALIBRATED against books whose state is known independently — these are the
// measured figures, not estimates:
//
//	Free Will        landed minutes earlier      100.00%
//	Atlas Shrugged   landed minutes earlier      100.00%
//	438 Days         landed minutes earlier       99.97%
//	Handmaid's Tale  current, lowest of 57        99.26%   <- the noise floor
//	Da Vinci Code    known stale (older decode)   98.98%
//	Atlas Shrugged   pre-landing snapshot         92.19%   <- known stale
//
// A current book loses at most ~0.7% to the narrator announcements the import
// strips; a badly stale one loses whole percent.
//
// BE HONEST ABOUT THE MARGIN: between the lowest current book (99.26%) and the
// closest stale one (98.98%) there is only 0.28 of a percentage point, because Da
// Vinci's two decodes differ by barely a hundred words. 99% separates every case in
// this library, but it is a narrow gate, not a comfortable one — which is why
// MatchPercent is always reported alongside the verdict rather than the tier alone,
// and why a value in the high 98s deserves a human look rather than an automatic
// reimport.
const driftMinMatchPct = 0.99

// SidecarDrift is one work's verdict.
type SidecarDrift struct {
	WorkID       int64  `json:"work_id"`
	Title        string `json:"title"`
	State        string `json:"state"`
	SidecarPath  string `json:"sidecar_path,omitempty"`
	SidecarWords int     `json:"sidecar_words,omitempty"`
	DBWords      int     `json:"db_words,omitempty"`
	Editions     int     `json:"editions,omitempty"`
	GramsTotal   int     `json:"grams_total,omitempty"`
	GramsFound   int     `json:"grams_found,omitempty"`
	MatchPercent float64 `json:"match_percent,omitempty"`
	ChunksStale  bool    `json:"chunks_stale,omitempty"`
	Detail       string  `json:"detail,omitempty"`
}

// gramSet returns the set of n-gram hashes over a normalized word slice.
func gramSet(words []string, n int) map[uint64]struct{} {
	out := make(map[uint64]struct{}, len(words))
	for i := 0; i+n <= len(words); i++ {
		out[hashGram(words[i:i+n])] = struct{}{}
	}
	return out
}

// hashGram is FNV-1a over the words joined by a separator that cannot occur in
// normalized text, so distinct grams cannot collide by concatenation.
func hashGram(words []string) uint64 {
	var h uint64 = 1469598103934665603
	for i, w := range words {
		if i > 0 {
			h = (h ^ 0x1f) * 1099511628211
		}
		for j := 0; j < len(w); j++ {
			h = (h ^ uint64(w[j])) * 1099511628211
		}
	}
	return h
}

var driftNormRe = regexp.MustCompile(`[^a-z0-9 ]+`)

func driftNorm(s string) string {
	s = strings.ToLower(s)
	s = driftNormRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// DetectSidecarDrift compares a work's on-disk sidecar against the transcript
// actually stored in the database. Read-only.
func DetectSidecarDrift(store *db.Store, libraryRoot string, workID int64) (*SidecarDrift, error) {
	w, err := store.GetWork(workID)
	if err != nil || w == nil {
		return nil, err
	}
	out := &SidecarDrift{WorkID: workID, Title: w.Title, State: DriftNoSidecar}
	if len(w.AudioFiles) == 0 {
		return out, nil
	}

	// A work can carry SEVERAL audio editions — a LibriVox reading and a TTS
	// edition, or two works merged — each with its own sidecar and its own
	// transcript book. Taking AudioFiles[0] and the first transcript book pairs
	// them arbitrarily: on Call of the Wild that compared the TTS edition's sidecar
	// against the LibriVox transcript and reported 76%, when each edition is
	// independently a perfect match. So every sidecar is matched to the transcript
	// it actually explains, and each transcript can only answer for one sidecar.
	seen := map[string]bool{}
	var sidecars []string
	for _, af := range w.AudioFiles {
		p := findSidecar(af.Path, libraryRoot)
		if p != "" && !seen[p] {
			seen[p] = true
			sidecars = append(sidecars, p)
		}
	}
	if len(sidecars) == 0 {
		return out, nil
	}
	out.SidecarPath = sidecars[0]
	out.Editions = len(sidecars)

	var transcripts []int64
	for _, b := range w.TextFiles {
		if b.Format == "transcript" || b.Origin == "whisper_transcript" {
			transcripts = append(transcripts, b.ID)
		}
	}
	if len(transcripts) == 0 {
		out.State = DriftNotImported
		out.Detail = "sidecar on disk but the work has no transcript book — never imported"
		return out, nil
	}

	// Load each transcript once.
	storedGrams := make([]map[uint64]struct{}, len(transcripts))
	storedWords := make([]int, len(transcripts))
	for i, tb := range transcripts {
		chapters, err := store.ListChapters(tb)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		for _, ch := range chapters {
			full, err := store.GetChapterContent(tb, ch.Index)
			if err != nil || full == nil {
				continue
			}
			sb.WriteString(full.Content)
			sb.WriteByte(' ')
		}
		words := strings.Fields(driftNorm(sb.String()))
		storedWords[i] = len(words)
		storedGrams[i] = gramSet(words, driftGram)
	}

	// match[i][j] = share of sidecar i's n-grams present in transcript j.
	match := make([][]float64, len(sidecars))
	gramsTotal := make([]int, len(sidecars))
	sidecarWordCount := make([]int, len(sidecars))
	for i, p := range sidecars {
		match[i] = make([]float64, len(transcripts))
		raw, err := os.ReadFile(p)
		if err != nil {
			out.State, out.Detail = DriftUnreadable, err.Error()
			return out, nil
		}
		var sc sttSidecar
		if err := json.Unmarshal(raw, &sc); err != nil {
			out.State, out.Detail = DriftUnreadable, "parse: "+err.Error()
			return out, nil
		}
		if len(sc.Words) == 0 {
			out.State, out.Detail = DriftUnreadable, "sidecar has no words"
			return out, nil
		}
		sidecarWordCount[i] = len(sc.Words)
		words := strings.Fields(driftNorm(strings.Join(wordStrings(sc.Words), " ")))
		if len(words) < driftGram {
			out.State, out.Detail = DriftUnreadable, "sidecar too short to compare"
			return out, nil
		}
		for j := range transcripts {
			var found, total int
			for k := 0; k+driftGram <= len(words); k++ {
				total++
				if _, ok := storedGrams[j][hashGram(words[k:k+driftGram])]; ok {
					found++
				}
			}
			gramsTotal[i] = total
			if total > 0 {
				match[i][j] = 100 * float64(found) / float64(total)
			}
		}
	}

	// Greedy assignment, best pair first, each transcript claimed once.
	assigned := make([]int, len(sidecars))
	for i := range assigned {
		assigned[i] = -1
	}
	usedT := make([]bool, len(transcripts))
	for range sidecars {
		bi, bj, best := -1, -1, -1.0
		for i := range sidecars {
			if assigned[i] != -1 {
				continue
			}
			for j := range transcripts {
				if usedT[j] {
					continue
				}
				if match[i][j] > best {
					bi, bj, best = i, j, match[i][j]
				}
			}
		}
		if bi < 0 {
			break
		}
		assigned[bi], usedT[bj] = bj, true
	}

	// The work is only as current as its worst edition.
	worst, worstIdx := 101.0, 0
	for i := range sidecars {
		m := 0.0
		if assigned[i] >= 0 {
			m = match[i][assigned[i]]
		}
		if m < worst {
			worst, worstIdx = m, i
		}
	}
	out.SidecarPath = sidecars[worstIdx]
	out.SidecarWords = sidecarWordCount[worstIdx]
	out.GramsTotal = gramsTotal[worstIdx]
	out.MatchPercent = worst
	out.GramsFound = int(worst / 100 * float64(gramsTotal[worstIdx]))
	if assigned[worstIdx] >= 0 {
		out.DBWords = storedWords[assigned[worstIdx]]
		stale, err := ChunksStale(store, transcripts[assigned[worstIdx]])
		if err != nil {
			return nil, err
		}
		out.ChunksStale = stale
	}

	switch {
	case assigned[worstIdx] < 0:
		out.State = DriftNotImported
		out.Detail = fmt.Sprintf("%s has no transcript book of its own — %d edition(s), %d transcript(s)",
			pathBase(sidecars[worstIdx]), len(sidecars), len(transcripts))
	case out.MatchPercent < driftMinMatchPct*100:
		out.State = DriftStale
		out.Detail = fmt.Sprintf("only %.2f%% of %s's %d-word sequences appear in the transcript it "+
			"produced — the database holds a different decode",
			out.MatchPercent, pathBase(sidecars[worstIdx]), driftGram)
	case out.ChunksStale:
		out.State = DriftChunksOnly
		out.Detail = "transcript matches the sidecar but the retrieval chunks do not — " +
			"the reader is correct while search and Q&A are not"
	default:
		out.State = DriftOK
	}
	return out, nil
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func wordStrings(ws []sttWord) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Word)
	}
	return out
}

// ChunksStale exposes the chunk-freshness check for reporting, without the
// rebuild ChunkBook would perform.
func ChunksStale(store *db.Store, bookID int64) (bool, error) {
	n, err := store.ChunkCount(bookID)
	if err != nil || n == 0 {
		return false, err
	}
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return false, err
	}
	return chunksStale(store, bookID, chapters)
}
