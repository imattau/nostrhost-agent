package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxKnowledgeDocuments = 10000
	maxKnowledgeDocBytes  = 64 * 1024
	maxKnowledgeResults   = 5
	maxKnowledgeExcerpt   = 4096
)

var knowledgeTokenPattern = regexp.MustCompile(`[\pL\pN]+`)

// KnowledgeDocument is an operator-selected local reference or verified
// operational record. Callers should exclude secrets and unverified outcomes.
type KnowledgeDocument struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

// KnowledgeMatch carries bounded untrusted text for the planner and a stable
// hash that lets the trace identify the exact indexed document version.
type KnowledgeMatch struct {
	ID     string  `json:"id"`
	Source string  `json:"source"`
	Hash   string  `json:"hash"`
	Score  float64 `json:"score"`
	Text   string  `json:"text"`
}

type KnowledgeCitation struct {
	ID     string  `json:"id"`
	Source string  `json:"source"`
	Hash   string  `json:"hash"`
	Score  float64 `json:"score"`
}

type Retriever interface {
	Retrieve(context.Context, string, int) ([]KnowledgeMatch, error)
}

type indexedKnowledge struct {
	document KnowledgeDocument
	hash     string
	terms    map[string]int
	length   int
}

// LocalRetriever provides dependency-free lexical retrieval over explicitly
// supplied local documents. It is intentionally in-memory; callers own the
// local corpus and persistence lifecycle.
type LocalRetriever struct {
	documents         []indexedKnowledge
	avgLength         float64
	documentFrequency map[string]int
}

func NewLocalRetriever(documents []KnowledgeDocument) (*LocalRetriever, error) {
	if len(documents) == 0 || len(documents) > maxKnowledgeDocuments {
		return nil, errors.New("knowledge corpus must contain between 1 and 10000 documents")
	}
	seen := make(map[string]struct{}, len(documents))
	index := &LocalRetriever{
		documents:         make([]indexedKnowledge, 0, len(documents)),
		documentFrequency: make(map[string]int),
	}
	for _, document := range documents {
		if strings.TrimSpace(document.ID) == "" || strings.TrimSpace(document.Source) == "" || len(document.Text) == 0 || len(document.Text) > maxKnowledgeDocBytes {
			return nil, errors.New("knowledge documents require an id, source, and text of at most 64 KiB")
		}
		if _, exists := seen[document.ID]; exists {
			return nil, fmt.Errorf("duplicate knowledge document id %q", document.ID)
		}
		seen[document.ID] = struct{}{}
		terms := termCounts(document.Text)
		length := 0
		for _, count := range terms {
			length += count
		}
		digest := sha256.Sum256([]byte(document.Text))
		index.documents = append(index.documents, indexedKnowledge{
			document: document,
			hash:     hex.EncodeToString(digest[:]),
			terms:    terms,
			length:   length,
		})
		for term := range terms {
			index.documentFrequency[term]++
		}
		index.avgLength += float64(length)
	}
	index.avgLength /= float64(len(index.documents))
	return index, nil
}

func (r *LocalRetriever) Retrieve(ctx context.Context, query string, limit int) ([]KnowledgeMatch, error) {
	return r.retrieve(ctx, query, limit, maxKnowledgeResults)
}

func (r *LocalRetriever) retrieve(ctx context.Context, query string, limit, maxLimit int) ([]KnowledgeMatch, error) {
	if r == nil || len(r.documents) == 0 {
		return nil, errors.New("local retriever is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = maxKnowledgeResults
	} else if limit > maxLimit {
		limit = maxLimit
	}
	queryTerms := termCounts(query)
	if len(queryTerms) == 0 {
		return nil, nil
	}
	type scored struct {
		match KnowledgeMatch
		index int
	}
	matches := make([]scored, 0, len(r.documents))
	for i, document := range r.documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		score := 0.0
		for term, queryFrequency := range queryTerms {
			tf := document.terms[term]
			if tf == 0 {
				continue
			}
			df := r.documentFrequency[term]
			idf := 1 + (float64(len(r.documents)-df)+0.5)/(float64(df)+0.5)
			lengthNorm := float64(document.length) / max(r.avgLength, 1)
			bm25 := idf * (float64(tf) * 2.2) / (float64(tf) + 1.2*(0.25+0.75*lengthNorm))
			score += bm25 * float64(queryFrequency)
		}
		if score == 0 {
			continue
		}
		text := document.document.Text
		if len(text) > maxKnowledgeExcerpt {
			text = text[:maxKnowledgeExcerpt]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		matches = append(matches, scored{index: i, match: KnowledgeMatch{
			ID: document.document.ID, Source: document.document.Source,
			Hash: document.hash, Score: score, Text: text,
		}})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].match.Score == matches[j].match.Score {
			return matches[i].index < matches[j].index
		}
		return matches[i].match.Score > matches[j].match.Score
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]KnowledgeMatch, len(matches))
	for i, match := range matches {
		result[i] = match.match
	}
	return result, nil
}

func termCounts(text string) map[string]int {
	counts := make(map[string]int)
	for _, token := range knowledgeTokenPattern.FindAllString(strings.ToLower(text), -1) {
		if len(token) > 64 {
			continue
		}
		counts[token]++
	}
	return counts
}
