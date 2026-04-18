package llm

import (
	"math"
	"testing"
)

func TestEncodeDecodeEmbedding(t *testing.T) {
	original := []float32{0.1, -0.5, 3.14, 0.0, -1.0}
	encoded := EncodeEmbedding(original)
	decoded := DecodeEmbedding(encoded)
	if len(decoded) != len(original) {
		t.Fatalf("length: want %d, got %d", len(original), len(decoded))
	}
	for i, v := range original {
		if decoded[i] != v {
			t.Errorf("value[%d]: want %v, got %v", i, v, decoded[i])
		}
	}
}

func TestDecodeEmbedding_BadLength(t *testing.T) {
	if got := DecodeEmbedding([]byte{1, 2, 3}); got != nil {
		t.Errorf("odd-length bytes should return nil, got %v", got)
	}
}

func TestCosineSimilarity_Identical(t *testing.T) {
	v := []float32{1, 2, 3, 4}
	sim := CosineSimilarity(v, v)
	if math.Abs(sim-1.0) > 1e-6 {
		t.Errorf("identical vectors: sim=%f, want 1.0", sim)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim := CosineSimilarity(a, b)
	if math.Abs(sim) > 1e-6 {
		t.Errorf("orthogonal vectors: sim=%f, want 0.0", sim)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{-1, -2, -3}
	sim := CosineSimilarity(a, b)
	if math.Abs(sim-(-1.0)) > 1e-6 {
		t.Errorf("opposite vectors: sim=%f, want -1.0", sim)
	}
}

func TestCosineSimilarity_DifferentLength(t *testing.T) {
	if sim := CosineSimilarity([]float32{1}, []float32{1, 2}); sim != 0 {
		t.Errorf("different-length: sim=%f, want 0", sim)
	}
}

func TestCosineSimilarity_Empty(t *testing.T) {
	if sim := CosineSimilarity(nil, nil); sim != 0 {
		t.Errorf("empty: sim=%f, want 0", sim)
	}
}
