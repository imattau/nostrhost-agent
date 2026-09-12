package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// small lexical index per query so new completed cycles become available
// immediately without persisting a second copy of operational history.
type VerifiedHistoryRetriever struct {
	corpus []KnowledgeDocument
	audit  AuditRecordReader
}

func NewVerifiedHistoryRetriever(corpus []KnowledgeDocument, audit AuditRecordReader) (*VerifiedHistoryRetriever, error) {
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
	return &VerifiedHistoryRetriever{corpus: append([]KnowledgeDocument(nil), corpus...), audit: audit}, nil
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
	return index.Retrieve(ctx, query, limit)
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
