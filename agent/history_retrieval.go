package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

const maxVerifiedHistoryDocuments = 1000
const auditRecordsForHistory = 10000

// AuditRecordReader supplies bounded recent persisted snapshots.
type AuditRecordReader interface {
	RecentRecords(context.Context, int) ([]CycleTrace, error)
}

// VerifiedHistoryRetriever combines an operator-selected corpus with recent,
// positively verified actions from the local audit journal. It rebuilds the
// lexical index per query so new completed cycles are available immediately;
// an optional local embedder reranks a bounded candidate set in memory.
type VerifiedHistoryRetriever struct {
	corpus     []KnowledgeDocument
	audit      AuditRecordReader
	embedder   Embedder
	vectorMu   sync.Mutex
	vectors    map[string][]float32
	failures   uint8
	retryAfter time.Time
}

func NewVerifiedHistoryRetriever(corpus []KnowledgeDocument, audit AuditRecordReader, embedders ...Embedder) (*VerifiedHistoryRetriever, error) {
	if len(embedders) > 1 {
		return nil, errors.New("configure at most one semantic embedder")
	}
	if len(corpus) > maxKnowledgeDocuments {
		return nil, errors.New("knowledge corpus cannot contain more than 10000 documents")
	}
	if len(corpus) > 0 {
		if _, err := NewLocalRetriever(corpus); err != nil {
			return nil, fmt.Errorf("invalid static knowledge corpus: %w", err)
		}
	}
	if audit == nil {
		return nil, errors.New("verified history retriever requires an audit reader")
	}
	var embedder Embedder
	if len(embedders) == 1 {
		embedder = embedders[0]
	}
	return &VerifiedHistoryRetriever{
		corpus: append([]KnowledgeDocument(nil), corpus...), audit: audit,
		embedder: embedder, vectors: make(map[string][]float32),
	}, nil
}

func (r *VerifiedHistoryRetriever) Retrieve(ctx context.Context, query string, limit int) ([]KnowledgeMatch, error) {
	if r == nil || r.audit == nil {
		return nil, errors.New("verified history retriever is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("verified history retrieval requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := r.audit.RecentRecords(ctx, auditRecordsForHistory)
	if err != nil {
		return nil, fmt.Errorf("read audit history for retrieval: %w", err)
	}
	remaining := maxKnowledgeDocuments - len(r.corpus)
	if remaining > maxVerifiedHistoryDocuments {
		remaining = maxVerifiedHistoryDocuments
	}
	documents := make([]KnowledgeDocument, 0, len(r.corpus)+remaining)
	documents = append(documents, r.corpus...)
	documents = append(documents, verifiedHistoryDocuments(records, remaining)...)
	if len(documents) == 0 {
		return nil, nil
	}
	index, err := NewLocalRetriever(documents)
	if err != nil {
		return nil, fmt.Errorf("index verified operational history: %w", err)
	}
	if r.embedder != nil {
		if !r.semanticCoolingDown() {
			matches, semanticErr := r.semanticRetrieve(ctx, index, query, limit)
			if semanticErr == nil {
				r.clearSemanticFailure()
				return matches, nil
			}
			if ctx.Err() != nil {
				return nil, semanticErr
			}
			r.noteSemanticFailure()
		}
	}
	return index.Retrieve(ctx, query, limit)
}

func (r *VerifiedHistoryRetriever) semanticCoolingDown() bool {
	r.vectorMu.Lock()
	defer r.vectorMu.Unlock()
	return time.Now().Before(r.retryAfter)
}

func (r *VerifiedHistoryRetriever) noteSemanticFailure() {
	r.vectorMu.Lock()
	defer r.vectorMu.Unlock()
	if r.failures < 5 {
		r.failures++
	}
	delay := time.Minute * time.Duration(1<<(r.failures-1))
	if delay > 16*time.Minute {
		delay = 16 * time.Minute
	}
	r.retryAfter = time.Now().Add(delay)
}

func (r *VerifiedHistoryRetriever) clearSemanticFailure() {
	r.vectorMu.Lock()
	r.failures = 0
	r.retryAfter = time.Time{}
	r.vectorMu.Unlock()
}

func (r *VerifiedHistoryRetriever) semanticRetrieve(ctx context.Context, index *LocalRetriever, query string, limit int) ([]KnowledgeMatch, error) {
	if len(index.documents) == 0 || len(termCounts(query)) == 0 {
		return nil, nil
	}
	lexical, err := index.retrieve(ctx, query, len(index.documents), maxKnowledgeDocuments)
	if err != nil {
		return nil, err
	}
	lexicalScores := make(map[string]float64, len(lexical))
	maxLexical := 0.0
	for _, match := range lexical {
		lexicalScores[match.ID] = match.Score
		if match.Score > maxLexical {
			maxLexical = match.Score
		}
	}
	selected := semanticCandidateIndices(index, lexical, maxSemanticIndexDocs)
	if len(selected) == 0 {
		return nil, nil
	}
	semanticCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	inputs := make([]string, 0, len(selected))
	inputIDs := make([]string, 0, len(selected))
	known := make(map[string][]float32, len(selected))
	r.vectorMu.Lock()
	active := make(map[string]struct{}, len(selected))
	for _, docIndex := range selected {
		document := index.documents[docIndex]
		active[document.hash] = struct{}{}
		if vector, exists := r.vectors[document.hash]; exists {
			known[document.hash] = vector
			continue
		}
		inputs = append(inputs, truncateEmbeddingText(document.document.Text))
		inputIDs = append(inputIDs, document.hash)
	}
	for key := range r.vectors {
		if _, keep := active[key]; !keep {
			delete(r.vectors, key)
		}
	}
	if len(inputs) > 0 {
		vectors, embedErr := r.embedder.Embed(semanticCtx, inputs)
		if embedErr != nil || len(vectors) != len(inputs) {
			r.vectorMu.Unlock()
			if embedErr != nil {
				return nil, embedErr
			}
			return nil, errors.New("embedder returned a mismatched number of document vectors")
		}
		for i, key := range inputIDs {
			normalized, normalizeErr := normalizeEmbeddingVector(vectors[i])
			if normalizeErr != nil || (len(r.vectors) > 0 && len(normalized) != len(firstVector(r.vectors))) {
				r.vectorMu.Unlock()
				return nil, errors.New("embedder returned an invalid document vector")
			}
			r.vectors[key] = normalized
			known[key] = normalized
		}
	}
	queryVectors, queryErr := r.embedder.Embed(semanticCtx, []string{truncateEmbeddingText(query)})
	if queryErr != nil || len(queryVectors) != 1 || len(queryVectors[0]) == 0 {
		r.vectorMu.Unlock()
		if queryErr != nil {
			return nil, queryErr
		}
		return nil, errors.New("embedder returned an invalid query vector")
	}
	queryVector, normalizeErr := normalizeEmbeddingVector(queryVectors[0])
	if normalizeErr != nil {
		r.vectorMu.Unlock()
		return nil, errors.New("embedder returned an invalid query vector")
	}
	for _, vector := range known {
		if len(vector) != len(queryVector) {
			r.vectorMu.Unlock()
			return nil, errors.New("embedding dimensions changed between requests")
		}
	}
	semanticScores := make(map[string]float64, len(known))
	for key, vector := range known {
		semanticScores[key] = cosineSimilarity(queryVector, vector)
	}
	r.vectorMu.Unlock()

	maxResults := limit
	if maxResults <= 0 || maxResults > maxKnowledgeResults {
		maxResults = maxKnowledgeResults
	}
	type ranked struct {
		match KnowledgeMatch
		index int
	}
	rankedMatches := make([]ranked, 0, len(index.documents))
	for i, document := range index.documents {
		lexicalScore := lexicalScores[document.document.ID]
		semanticScore, hasSemantic := semanticScores[document.hash]
		if !hasSemantic && lexicalScore == 0 {
			continue
		}
		score := 0.0
		if hasSemantic {
			score += 0.65 * math.Max(0, semanticScore)
		}
		if maxLexical > 0 {
			score += 0.35 * lexicalScore / maxLexical
		}
		if score == 0 {
			continue
		}
		text := document.document.Text
		if len(text) > maxKnowledgeExcerpt {
			text = truncateUTF8(text, maxKnowledgeExcerpt)
		}
		rankedMatches = append(rankedMatches, ranked{index: i, match: KnowledgeMatch{
			ID: document.document.ID, Source: document.document.Source,
			Hash: document.hash, Score: score, Text: text,
		}})
	}
	sort.Slice(rankedMatches, func(i, j int) bool {
		if rankedMatches[i].match.Score == rankedMatches[j].match.Score {
			return rankedMatches[i].index < rankedMatches[j].index
		}
		return rankedMatches[i].match.Score > rankedMatches[j].match.Score
	})
	if len(rankedMatches) > maxResults {
		rankedMatches = rankedMatches[:maxResults]
	}
	matches := make([]KnowledgeMatch, len(rankedMatches))
	for i := range rankedMatches {
		matches[i] = rankedMatches[i].match
	}
	return matches, nil
}

func semanticCandidateIndices(index *LocalRetriever, lexical []KnowledgeMatch, limit int) []int {
	selected := make(map[int]struct{}, limit)
	byID := make(map[string]int, len(index.documents))
	for i := range index.documents {
		byID[index.documents[i].document.ID] = i
	}
	lexicalLimit := limit / 2
	for i := 0; i < len(lexical) && i < lexicalLimit; i++ {
		if docIndex, exists := byID[lexical[i].ID]; exists {
			selected[docIndex] = struct{}{}
		}
	}
	for i := len(index.documents) - 1; i >= 0 && len(selected) < limit; i-- {
		selected[i] = struct{}{}
	}
	indices := make([]int, 0, len(selected))
	for i := range selected {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	return indices
}

func firstVector(vectors map[string][]float32) []float32 {
	for _, vector := range vectors {
		return vector
	}
	return nil
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) != len(right) || len(left) == 0 {
		return 0
	}
	dot := 0.0
	for i := range left {
		dot += float64(left[i] * right[i])
	}
	if math.IsNaN(dot) || math.IsInf(dot, 0) {
		return 0
	}
	return dot
}

func normalizeEmbeddingVector(vector []float32) ([]float32, error) {
	if len(vector) == 0 || len(vector) > maxEmbeddingDimensions {
		return nil, errors.New("embedding vector has invalid dimensions")
	}
	norm := 0.0
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("embedding vector contains a non-finite value")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 || math.IsInf(norm, 0) {
		return nil, errors.New("embedding vector has zero or invalid norm")
	}
	result := append([]float32(nil), vector...)
	normRoot := math.Sqrt(norm)
	for i := range result {
		result[i] = float32(float64(result[i]) / normRoot)
	}
	return result, nil
}

func verifiedHistoryDocuments(records []CycleTrace, limit int) []KnowledgeDocument {
	if limit <= 0 {
		return nil
	}
	documents := make([]KnowledgeDocument, 0, min(limit, len(records)))
	for i := len(records) - 1; i >= 0 && len(documents) < limit; i-- {
		trace := records[i]
		if trace.Result != "verified" || trace.ID == "" {
			continue
		}
		for proposalIndex := len(trace.Proposals) - 1; proposalIndex >= 0 && len(documents) < limit; proposalIndex-- {
			record := trace.Proposals[proposalIndex]
			if record.Outcome != "verified" || record.Verified == nil || !*record.Verified {
				continue
			}
			entry := struct {
				Trigger      string         `json:"trigger"`
				Target       string         `json:"target,omitempty"`
				Observations map[string]any `json:"observations,omitempty"`
				Operation    string         `json:"operation"`
				Arguments    map[string]any `json:"arguments,omitempty"`
				Result       map[string]any `json:"result,omitempty"`
			}{
				Trigger: trace.Trigger, Target: trace.Target,
				Observations: sanitizeMap(trace.Observations, nil),
				Operation:    record.Proposal.Operation,
				Arguments:    sanitizeMap(record.Proposal.Args, nil),
				Result:       sanitizeMap(record.Result, nil),
			}
			encoded, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			text := string(encoded)
			if len(text) > maxKnowledgeDocBytes {
				text = truncateUTF8(text, maxKnowledgeDocBytes)
			}
			documents = append(documents, KnowledgeDocument{
				ID:     "history-" + trace.ID + "-" + fmt.Sprint(proposalIndex),
				Source: "verified_history", Text: text,
			})
		}
	}
	// Records are returned oldest-first. Keep the newest verified examples but
	// restore chronological order so equal-score retrieval remains stable.
	for left, right := 0, len(documents)-1; left < right; left, right = left+1, right-1 {
		documents[left], documents[right] = documents[right], documents[left]
	}
	return documents
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
