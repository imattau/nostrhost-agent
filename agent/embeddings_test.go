package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAICompatibleEmbedderBatchesAndNormalizesVectors(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/v1/embeddings" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer local-secret" {
			t.Errorf("unexpected embedding request: path=%s method=%s auth=%q", r.URL.Path, r.Method, r.Header.Get("Authorization"))
		}
		var request struct {
			Model          string   `json:"model"`
			Input          []string `json:"input"`
			EncodingFormat string   `json:"encoding_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode embedding request: %v", err)
		}
		if request.Model != "local-embed" || request.EncodingFormat != "float" {
			t.Errorf("unexpected embedding request payload: %#v", request)
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{3, 4}}
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		return plannerResponse(http.StatusOK, string(body), r), nil
	})}
	embedder, err := NewOpenAICompatibleEmbedder(EmbeddingConfig{
		BaseURL: "http://127.0.0.1:8081/v1", Model: "local-embed", APIKey: "local-secret", Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]string, maxEmbeddingBatch+1)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("incident %d", i)
	}
	vectors, err := embedder.Embed(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(vectors) != len(inputs) {
		t.Fatalf("embedding batches = %d, vectors = %d", requests, len(vectors))
	}
	if len(vectors[0]) != 2 || vectors[0][0] != 0.6 || vectors[0][1] != 0.8 {
		t.Fatalf("embedding vector was not normalized: %#v", vectors[0])
	}
}

func TestOpenAICompatibleEmbedderRejectsRemoteEndpointsAndInvalidVectors(t *testing.T) {
	if _, err := NewOpenAICompatibleEmbedder(EmbeddingConfig{BaseURL: "https://example.com", Model: "remote"}); err == nil {
		t.Fatal("remote embedding endpoint accepted")
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return plannerResponse(http.StatusOK, `{"data":[{"index":0,"embedding":[0,0]}]}`, r), nil
	})}
	embedder, err := NewOpenAICompatibleEmbedder(EmbeddingConfig{BaseURL: "http://127.0.0.1:8081", Model: "local", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"test"}); err == nil {
		t.Fatal("zero embedding vector accepted")
	}
	redirects := 0
	redirectClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		redirects++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://example.com/collect"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	redirectEmbedder, err := NewOpenAICompatibleEmbedder(EmbeddingConfig{BaseURL: "http://127.0.0.1:8081", Model: "local", Client: redirectClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redirectEmbedder.Embed(context.Background(), []string{"test"}); err == nil || redirects != 1 {
		t.Fatalf("local embedding endpoint redirect was followed: err=%v requests=%d", err, redirects)
	}
}

type fakeEmbedder struct {
	calls int
	texts []string
	fn    func(string) []float32
	err   error
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		e.texts = append(e.texts, text)
		vectors[i] = e.fn(text)
	}
	return vectors, nil
}

func TestVerifiedHistoryRetrieverUsesSemanticRankingAndCachesVectors(t *testing.T) {
	embedder := &fakeEmbedder{fn: func(text string) []float32 {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "postgres data mount") || strings.Contains(lower, "database storage volume") {
			return []float32{1, 0}
		}
		return []float32{0, 1}
	}}
	retriever, err := NewVerifiedHistoryRetriever([]KnowledgeDocument{
		{ID: "proxy-guide", Source: "docs", Text: "Reverse proxy routes and certificate renewal."},
		{ID: "database-guide", Source: "docs", Text: "Database storage volume missing causes PostgreSQL startup failure."},
	}, &testAuditReader{}, embedder)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), "postgres data mount disappeared", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 || matches[0].ID != "database-guide" {
		t.Fatalf("semantic results did not surface the conceptually related document: %#v texts=%#v calls=%d", matches, embedder.texts, embedder.calls)
	}
	if embedder.calls != 2 {
		t.Fatalf("first query should embed corpus and query, calls=%d", embedder.calls)
	}
	if _, err := retriever.Retrieve(context.Background(), "postgres data mount unavailable", 5); err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 3 {
		t.Fatalf("document vectors were not cached across queries, calls=%d", embedder.calls)
	}
}

func TestVerifiedHistoryRetrieverFallsBackToLexicalWhenEmbeddingsFail(t *testing.T) {
	embedder := &fakeEmbedder{err: errors.New("embedding model unavailable")}
	retriever, err := NewVerifiedHistoryRetriever([]KnowledgeDocument{
		{ID: "proxy-guide", Source: "docs", Text: "Reverse proxy routes and certificate renewal."},
		{ID: "database-guide", Source: "docs", Text: "Database storage volume missing causes PostgreSQL startup failure."},
	}, &testAuditReader{}, embedder)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), "database storage volume", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 || matches[0].ID != "database-guide" {
		t.Fatalf("lexical fallback failed when semantic embeddings were unavailable: %#v", matches)
	}
	if _, err := retriever.Retrieve(context.Background(), "database storage volume", 5); err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 1 {
		t.Fatalf("failed semantic endpoint was retried during its cooldown: calls=%d", embedder.calls)
	}
}
