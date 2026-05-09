// Embedding for RAG vector search in Q&A. Supports two providers:
//   - OpenAI: text-embedding-3-small (1536 dim), batched, requires API key.
//   - Ollama: nomic-embed-text (768 dim), single-prompt, local-friendly.
//
// Embeddings are stored as raw float32 byte slices in the chunks table
// BLOB column. Cosine similarity is computed in pure Go. Vector dimensions
// are not assumed by the storage layer — `CosineSimilarity` requires equal
// lengths, so as long as a single library is embedded with one provider
// the math works out.
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
	OpenAIEmbedModel = "text-embedding-3-small"
	OllamaEmbedModel = "nomic-embed-text"
)

// EmbedRequest is a batch of texts to embed.
type EmbedRequest struct {
	Texts []string
}

// EmbedResponse contains the embeddings in the same order as the input texts.
type EmbedResponse struct {
	Embeddings [][]float32
	Model      string
	Usage      int // total tokens consumed (0 for Ollama)
}

// Embed dispatches to the configured provider's embedding endpoint.
func (c *Client) Embed(req EmbedRequest) (*EmbedResponse, error) {
	if len(req.Texts) == 0 {
		return &EmbedResponse{}, nil
	}
	switch c.provider {
	case ProviderOpenAI:
		return c.embedOpenAI(req)
	case ProviderOllama:
		return c.embedOllama(req)
	default:
		return nil, fmt.Errorf("embeddings not supported for provider %s", c.provider)
	}
}

func (c *Client) embedOpenAI(req EmbedRequest) (*EmbedResponse, error) {
	body := map[string]any{
		"input": req.Texts,
		"model": OpenAIEmbedModel,
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

// embedOllama calls Ollama's /api/embeddings, one text per request.
// Older Ollama versions don't support batching — we loop. Each call is
// fast (~50–200ms on a GPU-backed embedding model).
func (c *Client) embedOllama(req EmbedRequest) (*EmbedResponse, error) {
	embeddings := make([][]float32, len(req.Texts))
	for i, text := range req.Texts {
		body := map[string]any{
			"model":  OllamaEmbedModel,
			"prompt": text,
		}
		jsonBody, _ := json.Marshal(body)
		httpReq, _ := http.NewRequest("POST", c.baseURL+"/api/embeddings", bytes.NewReader(jsonBody))
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("ollama embed request %d: %w", i, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("ollama embed %d error %d: %s", i, resp.StatusCode, string(respBody))
		}
		var result struct {
			Embedding []float32 `json:"embedding"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("parse ollama embed response %d: %w", i, err)
		}
		embeddings[i] = result.Embedding
	}
	return &EmbedResponse{
		Embeddings: embeddings,
		Model:      OllamaEmbedModel,
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
