package llm

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestEmbedOutboundBoundary_TextOnly enforces, in code, that building the RAG
// index sends the configured provider EXACTLY the chunk texts + model id and
// nothing else — no title, author, path, or library metadata. Captures the real
// outbound body. If a future change widens the payload, this fails.
func TestEmbedOutboundBoundary_TextOnly(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"embedding": []float32{0.1, 0.2}, "index": 0}},
			"model": "text-embedding-3-small",
		})
	}))
	defer srv.Close()

	c := NewClient(ProviderOpenAI, "test-key", "", srv.URL)
	if _, err := c.Embed(EmbedRequest{Texts: []string{"the zorblatt engine glows blue"}}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	// The chunk text is present (the minimum needed to embed it)...
	if !strings.Contains(captured, "zorblatt engine glows blue") {
		t.Fatalf("chunk text missing from outbound body:\n%s", captured)
	}
	// ...and the parsed body has ONLY input + model keys — no smuggled metadata.
	var body map[string]any
	if err := json.Unmarshal([]byte(captured), &body); err != nil {
		t.Fatalf("parse captured body: %v", err)
	}
	for k := range body {
		if k != "input" && k != "model" {
			t.Fatalf("PRIVACY BOUNDARY: unexpected field %q in embeddings request (only input+model may leave): %s", k, captured)
		}
	}
}
