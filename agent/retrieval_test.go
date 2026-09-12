package agent

import (
	"context"
	"strings"
	"testing"
)

func TestLocalRetrieverRanksAndBoundsLocalDocuments(t *testing.T) {
	retriever, err := NewLocalRetriever([]KnowledgeDocument{
		{ID: "incident-db", Source: "verified-trace", Text: "PostgreSQL failed after storage mount disappeared; restoring mount recovered database health."},
		{ID: "guide-caddy", Source: "operator-guide", Text: "Caddy route checks inspect configured domains and certificate state."},
		{ID: "incident-generic", Source: "verified-trace", Text: "A service restart recovered a failed systemd unit."},
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), "PostgreSQL storage mount unavailable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != "incident-db" {
		t.Fatalf("unexpected retrieval ranking: %#v", matches)
	}
	if len(matches[0].Text) > maxKnowledgeExcerpt || len(matches[0].Hash) != 64 {
		t.Fatalf("retrieval result is not bounded or versioned: %#v", matches[0])
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := retriever.Retrieve(canceled, "database", 1); err == nil {
		t.Fatal("canceled retrieval was not interrupted")
	}
}

func TestLocalRetrieverRejectsInvalidCorpus(t *testing.T) {
	if _, err := NewLocalRetriever(nil); err == nil {
		t.Fatal("empty corpus accepted")
	}
	if _, err := NewLocalRetriever([]KnowledgeDocument{
		{ID: "same", Source: "a", Text: "one"},
		{ID: "same", Source: "b", Text: "two"},
	}); err == nil {
		t.Fatal("duplicate document id accepted")
	}
	if _, err := NewLocalRetriever([]KnowledgeDocument{{ID: "large", Source: "test", Text: strings.Repeat("x", maxKnowledgeDocBytes+1)}}); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestLocalRetrieverReturnsNoUnrelatedDocuments(t *testing.T) {
	retriever, err := NewLocalRetriever([]KnowledgeDocument{{ID: "guide", Source: "docs", Text: "Caddy manages reverse proxy routes."}})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), "unrelated PostgreSQL issue", 2)
	if err != nil || len(matches) != 0 {
		t.Fatalf("unrelated document returned: %#v, %v", matches, err)
	}
}
