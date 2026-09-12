package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type testAuditReader struct {
	records []CycleTrace
	err     error
}

func (r *testAuditReader) RecentRecords(context.Context, int) ([]CycleTrace, error) {
	return append([]CycleTrace(nil), r.records...), r.err
}

func verifiedHistoryTrace(id, status, argument string) CycleTrace {
	verified := true
	return CycleTrace{
		ID: id, Trigger: "health_check_failed", Target: "web",
		Observations: map[string]any{"service": status, "api_token": "history-secret"},
		Result:       "verified",
		Proposals: []ProposalRecord{{
			Proposal: Proposal{Operation: "service.restart", Args: map[string]any{"name": argument, "password": "history-secret"}},
			Outcome:  "verified", Verified: &verified,
			Result: map[string]any{"status": "active", "access_token": "history-secret"},
		}},
	}
}

func TestVerifiedHistoryRetrieverUsesOnlySuccessfulVerifiedTraces(t *testing.T) {
	verified := verifiedHistoryTrace("incident-good", "inactive", "web")
	unverified := verifiedHistoryTrace("incident-bad", "inactive", "database")
	unverified.Result = "needs_attention"
	unverified.Proposals[0].Outcome = "not_verified"
	reader := &testAuditReader{records: []CycleTrace{unverified, verified}}
	retriever, err := NewVerifiedHistoryRetriever(nil, reader)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := retriever.Retrieve(context.Background(), "inactive web service restart", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != "history-incident-good-0" || matches[0].Source != "verified_history" {
		t.Fatalf("retrieval included an ineligible history record: %#v", matches)
	}
	if strings.Contains(matches[0].Text, "history-secret") {
		t.Fatalf("sensitive history value reached retrieved knowledge: %s", matches[0].Text)
	}
	if strings.Count(matches[0].Text, redactedValue) < 3 {
		t.Fatalf("sensitive fields were not redacted in retrieved knowledge: %s", matches[0].Text)
	}
}

func TestVerifiedHistoryRetrieverSeesNewAuditRecordsOnNextQuery(t *testing.T) {
	reader := &testAuditReader{}
	retriever, err := NewVerifiedHistoryRetriever([]KnowledgeDocument{{
		ID: "operator-guide", Source: "local", Text: "Caddy route troubleshooting guide.",
	}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	first, err := retriever.Retrieve(context.Background(), "database storage mount", 5)
	if err != nil || len(first) != 0 {
		t.Fatalf("unexpected initial history retrieval: %#v, %v", first, err)
	}
	reader.records = []CycleTrace{verifiedHistoryTrace("incident-new", "postgresql storage mount missing", "postgresql")}
	second, err := retriever.Retrieve(context.Background(), "postgresql storage mount missing", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != "history-incident-new-0" {
		t.Fatalf("new verified audit record was not indexed: %#v", second)
	}
}

func TestVerifiedHistoryRetrieverPropagatesAuditFailures(t *testing.T) {
	retriever, err := NewVerifiedHistoryRetriever(nil, &testAuditReader{err: errors.New("audit unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retriever.Retrieve(context.Background(), "service failure", 5); err == nil {
		t.Fatal("audit reader failure was silently ignored")
	}
}
