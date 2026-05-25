package library

import (
	"encoding/binary"
	"math"
	"testing"
)

func encVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func TestDecodeNormalizeCosine(t *testing.T) {
	raw := encVec([]float32{3, 4}) // |v|=5
	v := normalizeVec(decodeVec(raw))
	if len(v) != 2 {
		t.Fatalf("decode len = %d", len(v))
	}
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Errorf("normalized = %v, want [0.6 0.8]", v)
	}
	// identical → 1, orthogonal → 0
	a := normalizeVec([]float32{1, 0, 0})
	b := normalizeVec([]float32{1, 0, 0})
	c := normalizeVec([]float32{0, 1, 0})
	if s := cosineNorm(a, b); math.Abs(s-1) > 1e-6 {
		t.Errorf("identical cosine = %f, want 1", s)
	}
	if s := cosineNorm(a, c); math.Abs(s) > 1e-6 {
		t.Errorf("orthogonal cosine = %f, want 0", s)
	}
	if cosineNorm(decodeVec(nil), a) != 0 {
		t.Errorf("decodeVec(nil) should yield cosine 0")
	}
}

// one-hot embChunk in dimension dim, with a global word span.
func oneHot(dim, n, gStart, gLen int) embChunk {
	v := make([]float32, n)
	v[dim] = 1
	return embChunk{gStart: gStart, gEnd: gStart + gLen, vec: normalizeVec(v)}
}

func TestEmbeddingChain_MonotonicMatches(t *testing.T) {
	// ebook chunks in dims 0..3; transcript matches e0, e2, e3 (e1 unmatched).
	ebook := []embChunk{oneHot(0, 4, 0, 10), oneHot(1, 4, 10, 10), oneHot(2, 4, 20, 10), oneHot(3, 4, 30, 10)}
	trans := []embChunk{oneHot(0, 4, 0, 10), oneHot(2, 4, 10, 10), oneHot(3, 4, 20, 10)}
	matches, meanSim := embeddingChain(ebook, trans, embSimThreshold)
	if len(matches) != 3 {
		t.Fatalf("want 3 matches, got %d: %+v", len(matches), matches)
	}
	wantE := []int{0, 2, 3}
	for i, m := range matches {
		if m.ebookIdx != wantE[i] || m.transIdx != i {
			t.Errorf("match %d = (e%d,t%d), want (e%d,t%d)", i, m.ebookIdx, m.transIdx, wantE[i], i)
		}
	}
	if math.Abs(meanSim-1) > 1e-6 {
		t.Errorf("meanSim = %f, want 1", meanSim)
	}
}

func TestEmbeddingChain_DifferentBook_NoMatches(t *testing.T) {
	// Every transcript chunk is orthogonal to every ebook chunk → nothing
	// clears the threshold → no alignment (the "genuinely different book" case).
	ebook := []embChunk{oneHot(0, 6, 0, 10), oneHot(1, 6, 10, 10)}
	trans := []embChunk{oneHot(3, 6, 0, 10), oneHot(4, 6, 10, 10)}
	matches, meanSim := embeddingChain(ebook, trans, embSimThreshold)
	if len(matches) != 0 {
		t.Errorf("expected no matches for orthogonal texts, got %d", len(matches))
	}
	if meanSim != 0 {
		t.Errorf("meanSim = %f, want 0", meanSim)
	}
}

func TestEmbeddingChain_RejectsOutOfOrder(t *testing.T) {
	// trans0→e2, trans1→e0 would be non-monotonic; the chain keeps one.
	ebook := []embChunk{oneHot(0, 3, 0, 10), oneHot(1, 3, 10, 10), oneHot(2, 3, 20, 10)}
	trans := []embChunk{oneHot(2, 3, 0, 10), oneHot(0, 3, 10, 10)}
	matches, _ := embeddingChain(ebook, trans, embSimThreshold)
	if len(matches) != 1 {
		t.Fatalf("want 1 (monotonic) match, got %d: %+v", len(matches), matches)
	}
}

func TestChunkSegments_AlignedAndGaps(t *testing.T) {
	ebook := []embChunk{oneHot(0, 4, 0, 10), oneHot(1, 4, 10, 10), oneHot(2, 4, 20, 10)}
	trans := []embChunk{oneHot(0, 4, 0, 10), oneHot(2, 4, 10, 10)}
	matches := []chunkMatch{{0, 0}, {2, 1}} // e0↔t0, e2↔t1; e1 unmatched
	segs := chunkSegments(ebook, trans, matches)
	var aligned, ebookOnly int
	pe, pt := 0, 0
	for _, s := range segs {
		if s.EbookEnd < s.EbookStart || s.TransEnd < s.TransStart {
			t.Fatalf("negative-width segment: %+v", s)
		}
		if s.EbookStart < pe || s.TransStart < pt {
			t.Fatalf("overlap: %+v after (e%d,t%d)", s, pe, pt)
		}
		pe, pt = s.EbookEnd, s.TransEnd
		switch s.Kind {
		case SegAligned:
			aligned++
		case SegEbookOnly:
			ebookOnly++
		}
	}
	if aligned != 2 {
		t.Errorf("want 2 aligned segments, got %d", aligned)
	}
	if ebookOnly < 1 {
		t.Errorf("want an ebook-only gap for the unmatched middle chunk, got %d", ebookOnly)
	}
}
