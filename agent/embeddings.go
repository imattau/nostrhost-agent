package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxEmbeddingBatch      = 32
	maxEmbeddingInputs     = 1024
	maxEmbeddingDimensions = 8192
	maxEmbeddingResponse   = 8 << 20
	maxSemanticIndexDocs   = 512
)

type EmbeddingConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	Client  *http.Client
}

// Embedder returns one normalized vector for each input text, in input order.
type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

// OpenAICompatibleEmbedder calls a local OpenAI-compatible /v1/embeddings
// endpoint. It sends bounded batches and never follows redirects off-host.
type OpenAICompatibleEmbedder struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

func NewOpenAICompatibleEmbedder(cfg EmbeddingConfig) (*OpenAICompatibleEmbedder, error) {
	endpoint, err := localEmbeddingsEndpoint(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("local embedding model name is required")
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	if client.Timeout <= 0 || client.Timeout > 2*time.Minute {
		client.Timeout = 2 * time.Minute
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &OpenAICompatibleEmbedder{endpoint: endpoint, model: cfg.Model, apiKey: cfg.APIKey, client: client}, nil
}

func (e *OpenAICompatibleEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("local embedder is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("embedding request requires a context")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > maxEmbeddingInputs {
		return nil, errors.New("embedding batch exceeds the document limit")
	}
	result := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += maxEmbeddingBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+maxEmbeddingBatch, len(texts))
		batch := make([]string, end-start)
		for i, text := range texts[start:end] {
			text = truncateEmbeddingText(text)
			if strings.TrimSpace(text) == "" {
				return nil, errors.New("embedding input text cannot be empty")
			}
			batch[i] = text
		}
		vectors, err := e.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		copy(result[start:end], vectors)
	}
	return result, nil
}

func (e *OpenAICompatibleEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(struct {
		Model          string   `json:"model"`
		Input          []string `json:"input"`
		EncodingFormat string   `json:"encoding_format"`
	}{Model: e.model, Input: texts, EncodingFormat: "float"})
	if err != nil || len(body) > maxInferenceRequestBytes {
		return nil, errors.New("local embedding request exceeds the size limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("could not create local embedding request")
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	response, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local embedding request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("local embedding endpoint returned HTTP %d", response.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxEmbeddingResponse+1))
	if err != nil || len(responseBody) > maxEmbeddingResponse {
		return nil, errors.New("local embedding response exceeds the size limit")
	}
	var payload struct {
		Data []struct {
			Index     *int      `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &payload); err != nil || len(payload.Data) != len(texts) {
		return nil, errors.New("local embedding endpoint returned an invalid response")
	}
	result := make([][]float32, len(texts))
	dimensions := 0
	for _, item := range payload.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(result) || len(result[*item.Index]) != 0 || len(item.Embedding) == 0 || len(item.Embedding) > maxEmbeddingDimensions {
			return nil, errors.New("local embedding endpoint returned invalid vector dimensions or indices")
		}
		if dimensions != 0 && len(item.Embedding) != dimensions {
			return nil, errors.New("local embedding endpoint returned inconsistent vector dimensions")
		}
		dimensions = len(item.Embedding)
		vector := make([]float32, len(item.Embedding))
		norm := 0.0
		for _, value := range item.Embedding {
			if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxFloat32 {
				return nil, errors.New("local embedding endpoint returned a non-finite vector value")
			}
			norm += value * value
		}
		if norm == 0 || math.IsInf(norm, 0) {
			return nil, errors.New("local embedding endpoint returned a zero or invalid vector")
		}
		normRoot := math.Sqrt(norm)
		for i, value := range item.Embedding {
			vector[i] = float32(value / normRoot)
		}
		result[*item.Index] = vector
	}
	for _, vector := range result {
		if len(vector) == 0 {
			return nil, errors.New("local embedding endpoint omitted a vector")
		}
	}
	return result, nil
}

func localEmbeddingsEndpoint(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !isLoopbackHost(parsed.Hostname()) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("embedding endpoint must use http/https on a loopback host without embedded credentials")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path != "" && path != "/v1" {
		return "", errors.New("embedding base URL path must be empty or /v1")
	}
	parsed.Path = "/v1/embeddings"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func truncateEmbeddingText(text string) string {
	if len(text) > maxKnowledgeExcerpt {
		text = text[:maxKnowledgeExcerpt]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}
