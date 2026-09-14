package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exportableTrace() CycleTrace {
	verified := true
	return CycleTrace{
		ID: "cycle-private-id", StartedAt: time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 9, 12, 1, 1, 0, 0, time.UTC),
		Trigger:    "health_check_failed for admin@example.com at https://server.example.com/path password=do-not-export",
		Target:     "photos.example.com", PlanningCompleted: true,
		Observations: map[string]any{"status": "failed", "host": "nostr.example.com", "api_token": "secret-value", "ipv6": "fd00::1234"},
		AvailableOperations: []OperationSnapshot{{
			Name: "service.restart", Description: "Restart one known service.",
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		}},
		Proposals: []ProposalRecord{{
			Proposal: Proposal{Operation: "service.restart", Args: map[string]any{"name": "private-service", "reason": "restart for 10.0.0.8"}},
			Policy:   PolicyResult{Decision: DecisionAllow, Reason: "reason has personal data"},
			Outcome:  "verified", Verified: &verified,
		}},
		Result: "verified", Resolution: "do not export this free-form resolution", Knowledge: []KnowledgeCitation{{ID: "private", Source: "/home/alice/incident.md"}},
	}
}

func TestBuildContributionCandidateRedactsAndLeavesExpectedDecisionUnlabeled(t *testing.T) {
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ExpectedDecision != nil || candidate.ReviewStatus != "submitted" || !candidate.PrivacyReviewRequired {
		t.Fatalf("candidate incorrectly represents a ground-truth label: %#v", candidate)
	}
	if candidate.ObservedDecision.NoCall || candidate.ObservedDecision.Operation != "service.restart" {
		t.Fatalf("planner output not preserved as observed output: %#v", candidate.ObservedDecision)
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"admin@example.com", "server.example.com", "10.0.0.8", "private-service", "secret-value", "do-not-export", "fd00::1234", "cycle-private-id", "do not export this free-form resolution", "incident.md"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("candidate leaked %q: %s", forbidden, text)
		}
	}
	if candidate.RedactionsApplied < 5 {
		t.Fatalf("expected privacy redactions, got %d", candidate.RedactionsApplied)
	}
	if len(candidate.PlannerInput.AvailableOperations) != 1 || !json.Valid(candidate.PlannerInput.AvailableOperations[0].ArgsSchema) {
		t.Fatalf("operation schema missing from planner input: %#v", candidate.PlannerInput.AvailableOperations)
	}
}

func TestBuildContributionCandidateDistinguishesCompletedNoCall(t *testing.T) {
	trace := exportableTrace()
	trace.Proposals = nil
	trace.Result = "completed"
	candidate, err := BuildContributionCandidate(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.ObservedDecision.NoCall || candidate.ObservedDecision.Operation != "" {
		t.Fatalf("completed no-call decision was not represented: %#v", candidate.ObservedDecision)
	}
	trace.PlanningCompleted = false
	if _, err := BuildContributionCandidate(trace); err == nil {
		t.Fatal("unplanned cycle accepted as a no-call")
	}
}

func TestBuildContributionCandidateDoesNotExportUnknownOperationText(t *testing.T) {
	trace := exportableTrace()
	trace.Proposals[0].Proposal.Operation = "operator@example.org"
	candidate, err := BuildContributionCandidate(trace)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ObservedDecision.Operation != "unknown" {
		t.Fatalf("unknown model text was preserved as an operation name: %#v", candidate.ObservedDecision)
	}
}

func TestReadContributionCycleSelectsLatestWithoutMutatingJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	first := exportableTrace()
	first.FinishedAt = time.Time{}
	first.PlanningCompleted = false
	final := exportableTrace()
	firstJSON, _ := json.Marshal(first)
	finalJSON, _ := json.Marshal(final)
	contents := append(append(firstJSON, '\n'), append(finalJSON, '\n')...)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	got, err := ReadContributionCycle(path, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PlanningCompleted || got.FinishedAt.IsZero() {
		t.Fatalf("did not return latest finalized cycle snapshot: %#v", got)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("read-only export changed the journal")
	}
}

func TestReadContributionCycleRejectsUnsafeOrIncompleteJournal(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadContributionCycle(link, "cycle-private-id"); err == nil {
			t.Fatal("symlink journal accepted")
		}
	})
	t.Run("permissive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit")
		data, _ := json.Marshal(exportableTrace())
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadContributionCycle(path, "cycle-private-id"); err == nil {
			t.Fatal("permissive journal accepted")
		}
	})
	t.Run("partial tail", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit")
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadContributionCycle(path, "cycle-private-id"); err == nil {
			t.Fatal("incomplete journal accepted")
		}
	})
	t.Run("missing cycle", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit")
		data, _ := json.Marshal(exportableTrace())
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadContributionCycle(path, "other-cycle"); err == nil {
			t.Fatal("unselected cycle accepted")
		}
	})
}

func TestWriteContributionCandidateIsPrivateAndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate.json")
	candidate, err := BuildContributionCandidate(exportableTrace())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteContributionCandidate(path, candidate); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %o, want 600", info.Mode().Perm())
	}
	if err := WriteContributionCandidate(path, candidate); err == nil {
		t.Fatal("existing output overwritten")
	}
}

func TestListExportableCyclesOnlyReturnsFinalizedCyclesWithoutContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	incomplete := exportableTrace()
	incomplete.ID = "cycle-incomplete"
	incomplete.FinishedAt = time.Time{}
	incomplete.PlanningCompleted = false
	finalized := exportableTrace()
	finalized.ID = "cycle-finalized"
	incompleteJSON, _ := json.Marshal(incomplete)
	finalizedJSON, _ := json.Marshal(finalized)
	contents := append(append(incompleteJSON, '\n'), append(finalizedJSON, '\n')...)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	summaries, err := ListExportableCycles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].CycleID != "cycle-finalized" {
		t.Fatalf("expected exactly the finalized cycle, got %#v", summaries)
	}
	if summaries[0].Decision != "service.restart" || summaries[0].CycleResult != "verified" {
		t.Fatalf("unexpected summary content: %#v", summaries[0])
	}
	encoded, _ := json.Marshal(summaries)
	for _, forbidden := range []string{"private-service", "10.0.0.8", "admin@example.com", "incident.md"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("cycle summary leaked observation/proposal content: %q in %s", forbidden, encoded)
		}
	}
}
