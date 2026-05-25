// Embedding-based alignment for the cross-translation case.
//
// When lexical anchoring collapses (different translation / edition of the
// same work — e.g. a modern Republic audiobook vs the Jowett text, which
// scores ~0% lexical coverage), the *content* still corresponds: paragraph
// embeddings of the two texts land at near-identical vectors despite zero
// shared phrasing. This aligns the two chunk sequences by cosine similarity
// along a monotonic chain (same shape as the lexical anchor chain, scored by
// similarity instead of exact match), so genuine divergences — the other 36
// dialogues in an all-dialogues EPUB — are left unaligned rather than forced
// into a full DTW path.
//
// Output is the SAME AnchorAlignmentPayload the lexical path emits (Unit
// "paragraph" instead of "word"; offsets are in the chunker's word basis),
// so the diff-view renders both uniformly. Stored as an alignments row with
// Method="embedding".
package library

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/pj/abookify/internal/db"
)

// ChunkEmbedder populates + persists embeddings for a book's chunks. Satisfied
// by *llm.RAG.EmbedBook — passed in so this package stays decoupled from llm.
type ChunkEmbedder interface {
	EmbedBook(bookID int64) (int, error)
}

// embSimThreshold is the minimum cosine for a chunk pair to count as a match.
// nomic-embed-text puts same-passage paraphrases ~0.75-0.95 and unrelated
// text ~0.3-0.5 (see the Plato prototype), so 0.6 cleanly separates them.
const embSimThreshold = 0.6

// embChunk is one paragraph-ish unit with its global word span (chunker
// basis) and L2-normalized embedding.
type embChunk struct {
	gStart, gEnd int
	vec          []float32
}

// ComputeEmbeddingAlignment aligns a work's ebook ↔ transcript by paragraph
// embeddings and upserts an alignments row (Method="embedding",
// Unit="paragraph"). Returns coverage (aligned ebook words / total) and
// matchQuality (mean cosine of the matched chain — high ⇒ same work in a
// different translation; low ⇒ genuinely different texts). No-op if a peer
// or its embeddings are missing.
func ComputeEmbeddingAlignment(store *db.Store, embedder ChunkEmbedder, workID int64) (coverage, matchQuality float64, err error) {
	work, err := store.GetWork(workID)
	if err != nil || work == nil {
		return 0, 0, err
	}
	var ebook, transcript *db.Book
	for i := range work.TextFiles {
		b := &work.TextFiles[i]
		switch b.Origin {
		case "whisper_transcript":
			if transcript == nil {
				transcript = b
			}
		case "publisher_epub", "publisher_mobi", "publisher_pdf":
			if ebook == nil || db.OriginAuthority(b.Origin) > db.OriginAuthority(ebook.Origin) {
				ebook = b
			}
		}
	}
	if ebook == nil || transcript == nil {
		return 0, 0, nil
	}

	// Populate + persist embeddings (reuses the RAG pipeline; also makes the
	// vectors available to Q&A). Idempotent — only embeds unembedded chunks.
	if embedder != nil {
		if _, err := embedder.EmbedBook(ebook.ID); err != nil {
			return 0, 0, fmt.Errorf("embed ebook: %w", err)
		}
		if _, err := embedder.EmbedBook(transcript.ID); err != nil {
			return 0, 0, fmt.Errorf("embed transcript: %w", err)
		}
	}

	ebChunks, ebSpans, ebWords, err := loadEmbChunks(store, ebook.ID, true)
	if err != nil {
		return 0, 0, fmt.Errorf("load ebook chunks: %w", err)
	}
	trChunks, trSpans, trWords, err := loadEmbChunks(store, transcript.ID, false)
	if err != nil {
		return 0, 0, fmt.Errorf("load transcript chunks: %w", err)
	}
	if len(ebChunks) == 0 || len(trChunks) == 0 {
		return 0, 0, nil // no embeddings yet
	}

	matches, meanSim := embeddingChain(ebChunks, trChunks, embSimThreshold)
	segs := chunkSegments(ebChunks, trChunks, matches)

	aligned := 0
	for _, s := range segs {
		if s.Kind == SegAligned {
			aligned += s.EbookEnd - s.EbookStart
		}
	}
	if ebWords > 0 {
		coverage = float64(aligned) / float64(ebWords)
	}
	if coverage > 1 {
		coverage = 1
	}
	matchQuality = meanSim

	payload := AnchorAlignmentPayload{
		EbookChapters: ebSpans,
		TransChapters: trSpans,
		Segments:      segs,
		EbookWords:    ebWords,
		TransWords:    trWords,
		Coverage:      coverage,
		MatchQuality:  matchQuality,
		Divergence:    summarizeAnchorDivergence(segs),
	}
	pairsJSON, err := json.Marshal(payload)
	if err != nil {
		return coverage, matchQuality, fmt.Errorf("marshal payload: %w", err)
	}
	if err := store.SaveAlignment(db.Alignment{
		WorkID:     workID,
		FromBookID: ebook.ID,
		ToBookID:   transcript.ID,
		Unit:       "paragraph",
		Confidence: coverage,
		Method:     "embedding",
		Pairs:      string(pairsJSON),
	}); err != nil {
		return coverage, matchQuality, fmt.Errorf("save alignment: %w", err)
	}
	return coverage, matchQuality, nil
}

// loadEmbChunks loads a book's embedded chunks in reading order, decodes +
// normalizes their vectors, and assigns each a global word span (chunker
// basis: chapter offset from word counts + chunk's within-chapter StartWord).
// Boilerplate chapters are dropped when dropBoilerplate is set. Also returns
// the ChapterSpan table and total content word count.
func loadEmbChunks(store *db.Store, bookID int64, dropBoilerplate bool) ([]embChunk, []ChapterSpan, int, error) {
	chapters, err := store.ListChapters(bookID)
	if err != nil {
		return nil, nil, 0, err
	}
	sort.Slice(chapters, func(i, j int) bool { return chapters[i].Index < chapters[j].Index })

	// Chapter global starts in the chunker's word basis (strings.Fields), and
	// the ChapterSpan table for the payload.
	chapterStart := map[int]int{}
	var spans []ChapterSpan
	total := 0
	for _, ch := range chapters {
		if dropBoilerplate && IsBoilerplateChapterTitle(ch.Title) {
			continue
		}
		full, err := store.GetChapterContent(bookID, ch.Index)
		if err != nil || full == nil {
			continue
		}
		wc := len(strings.Fields(full.Content))
		if wc == 0 {
			continue
		}
		chapterStart[ch.Index] = total
		spans = append(spans, ChapterSpan{Index: ch.Index, Start: total, Len: wc})
		total += wc
	}

	chunks, err := store.ListChunks(bookID)
	if err != nil {
		return nil, nil, 0, err
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].ChapterIdx != chunks[j].ChapterIdx {
			return chunks[i].ChapterIdx < chunks[j].ChapterIdx
		}
		return chunks[i].StartWord < chunks[j].StartWord
	})
	var out []embChunk
	for _, c := range chunks {
		base, ok := chapterStart[c.ChapterIdx]
		if !ok {
			continue // chunk in a dropped/boilerplate chapter
		}
		if len(c.Embedding) == 0 {
			continue
		}
		vec := normalizeVec(decodeVec(c.Embedding))
		if vec == nil {
			continue
		}
		out = append(out, embChunk{
			gStart: base + c.StartWord,
			gEnd:   base + c.EndWord,
			vec:    vec,
		})
	}
	return out, spans, total, nil
}

// embeddingChain matches each transcript chunk to its most-similar ebook
// chunk (when cosine ≥ threshold), then keeps the longest monotonic
// subsequence so the alignment advances in order on both sides. Reuses the
// lexical-anchor LIS (MonotonicChain). Returns matched (ebookIdx, transIdx)
// pairs in transcript order, plus the mean cosine of the kept matches.
func embeddingChain(ebook, trans []embChunk, threshold float64) (matches []chunkMatch, meanSim float64) {
	var cands []Anchor
	simByT := map[int]float64{}
	for ti, tc := range trans {
		bestE, bestSim := -1, threshold
		for ei, ec := range ebook {
			s := cosineNorm(tc.vec, ec.vec)
			if s >= bestSim {
				bestSim, bestE = s, ei
			}
		}
		if bestE >= 0 {
			cands = append(cands, Anchor{EbookPos: bestE, TransPos: ti})
			simByT[ti] = bestSim
		}
	}
	chain := MonotonicChain(cands)
	var sum float64
	for _, a := range chain {
		matches = append(matches, chunkMatch{ebookIdx: a.EbookPos, transIdx: a.TransPos})
		sum += simByT[a.TransPos]
	}
	if len(matches) > 0 {
		meanSim = sum / float64(len(matches))
	}
	return matches, meanSim
}

type chunkMatch struct{ ebookIdx, transIdx int }

// chunkSegments turns the matched chunk pairs into the same Segment stream the
// lexical path emits: SegAligned for matched chunk pairs, with the ebook-only
// / trans-only gaps between them. Positions are global word offsets (chunker
// basis). Starts are clamped so chunk-overlap can't create negative spans.
func chunkSegments(ebook, trans []embChunk, matches []chunkMatch) []Segment {
	var segs []Segment
	ePrev, tPrev := 0, 0
	flush := func(eAt, tAt int) {
		if eAt < ePrev {
			eAt = ePrev
		}
		if tAt < tPrev {
			tAt = tPrev
		}
		if kind, ok := classifyGap(eAt-ePrev, tAt-tPrev); ok {
			segs = append(segs, Segment{ePrev, eAt, tPrev, tAt, kind})
		}
	}
	for _, m := range matches {
		ec, tc := ebook[m.ebookIdx], trans[m.transIdx]
		flush(ec.gStart, tc.gStart)
		eS, tS := ec.gStart, tc.gStart
		if eS < ePrev {
			eS = ePrev
		}
		if tS < tPrev {
			tS = tPrev
		}
		if ec.gEnd <= eS && tc.gEnd <= tS {
			continue
		}
		segs = append(segs, Segment{eS, ec.gEnd, tS, tc.gEnd, SegAligned})
		if ec.gEnd > ePrev {
			ePrev = ec.gEnd
		}
		if tc.gEnd > tPrev {
			tPrev = tc.gEnd
		}
	}
	// Trailing gaps.
	eEnd, tEnd := ePrev, tPrev
	if len(ebook) > 0 && ebook[len(ebook)-1].gEnd > eEnd {
		eEnd = ebook[len(ebook)-1].gEnd
	}
	if len(trans) > 0 && trans[len(trans)-1].gEnd > tEnd {
		tEnd = trans[len(trans)-1].gEnd
	}
	flush(eEnd, tEnd)
	return segs
}

func decodeVec(b []byte) []float32 {
	if len(b)%4 != 0 || len(b) == 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func normalizeVec(v []float32) []float32 {
	if len(v) == 0 {
		return nil
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := math.Sqrt(sum)
	if n == 0 {
		return nil
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// cosineNorm is the dot product of two already-L2-normalized vectors.
func cosineNorm(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
