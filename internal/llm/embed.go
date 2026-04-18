// Embedding via OpenAI's text-embedding-3-small (BYOK). Used for RAG
// vector search in Q&A — requires the same OpenAI API key used for chat.
//
// Embeddings are stored as raw float32 byte slices in the chunks table
// BLOB column. Cosine similarity is computed in pure Go.
package llm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
)

const (
	EmbedModel     = "text-embedding-3-small"
	EmbedDimension = 1536 // text-embedding-3-small output dimension
)

// EmbedRequest is a batch of texts to embed.
type EmbedRequest struct {
	Texts []string
}

// EmbedResponse contains the embeddings in the same order as the input texts.
type EmbedResponse struct {
	Embeddings [][]float32
	Model      string
	Usage      int // total tokens consumed
}

// Embed calls OpenAI's embeddings endpoint. Only works when provider is
// OpenAI (needs the API key). Returns an error for other providers.
func (c *Client) Embed(req EmbedRequest) (*EmbedResponse, error) {
	if c.provider != ProviderOpenAI {
		return nil, fmt.Errorf("embeddings require OpenAI provider (have %s)", c.provider)
	}
	if len(req.Texts) == 0 {
		return &EmbedResponse{}, nil
	}

	body := map[string]any{
		"input": req.Texts,
		"model": EmbedModel,
	}
	jsonBody, _ := json.Marshal(body)
	httpReq, _ := http.NewRequest("POST", c.baseURL+"/v1/embeddings", bytes.NewReader(jsonBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embed response: %w", err)
	}

	embeddings := make([][]float32, len(req.Texts))
	for _, d := range result.Data {
		if d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}

	return &EmbedResponse{
		Embeddings: embeddings,
		Model:      result.Model,
		Usage:      result.Usage.TotalTokens,
	}, nil
}

// EncodeEmbedding serializes a float32 slice to bytes for BLOB storage.
func EncodeEmbedding(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// DecodeEmbedding deserializes a BLOB back to float32 slice.
func DecodeEmbedding(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 if either is empty or they differ in length.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
