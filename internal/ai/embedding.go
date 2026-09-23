package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// EmbeddingProvider converts text to vectors. Implementations must not retain text.
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
}

// MockEmbeddingProvider is deterministic and dependency-free for tests and development.
type MockEmbeddingProvider struct{ dimensions int }

func NewMockEmbeddingProvider(dimensions int) *MockEmbeddingProvider {
	if dimensions < 1 {
		dimensions = 8
	}
	return &MockEmbeddingProvider{dimensions: dimensions}
}
func (m *MockEmbeddingProvider) Dimensions() int { return m.dimensions }
func (m *MockEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		h := sha256.Sum256([]byte(text))
		v := make([]float32, m.dimensions)
		for j := range v {
			n := binary.BigEndian.Uint32(h[(j*4)%29:])
			v[j] = float32(int64(n%2000000)-1000000) / 1000000
		}
		norm := float32(0)
		for _, x := range v {
			norm += x * x
		}
		norm = float32(math.Sqrt(float64(norm)))
		if norm > 0 {
			for j := range v {
				v[j] /= norm
			}
		}
		out[i] = v
	}
	return out, nil
}

// OpenAIEmbeddingProvider implements the OpenAI-compatible /embeddings API.
type OpenAIEmbeddingProvider struct {
	baseURL, apiKey, model string
	client                 HTTPDoer
}
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func NewOpenAIEmbeddingProvider(baseURL, apiKey, model string, client HTTPDoer) *OpenAIEmbeddingProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAIEmbeddingProvider{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, model: model, client: client}
}
func (p *OpenAIEmbeddingProvider) Dimensions() int { return 0 }
func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	payload, err := json.Marshal(map[string]any{"model": p.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding status %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode embeddings: %w", err)
	}
	out := make([][]float32, len(texts))
	for _, item := range result.Data {
		if item.Index >= 0 && item.Index < len(out) {
			out[item.Index] = item.Embedding
		}
	}
	for _, vector := range out {
		if len(vector) == 0 {
			return nil, fmt.Errorf("embedding response missing vector")
		}
	}
	return out, nil
}
