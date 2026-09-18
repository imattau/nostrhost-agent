package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestAutoSubmitter(t *testing.T, dir, apiURL string) *ContributionAutoSubmitter {
	t.Helper()
	submitter, err := NewContributionAutoSubmitter(ContributionFileConfig{
		Enabled: true, DatasetRepo: "owner/repo", TokenPath: writeTestToken(t, dir),
		BaseRevision: "main", StatePath: filepath.Join(dir, "state.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if apiURL != "" {
		submitter.apiBaseURL = apiURL
	}
	return submitter
}

func TestContributionAutoSubmitterSubmitsEligibleCycleExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha", "https://github.com/imattau/nostrhost-contributions/pull/9", "", 0)
	defer server.Close()

	submitter := newTestAutoSubmitter(t, dir, server.URL)
	trace := exportableTrace()
	trace.ID = "cycle-auto-1"

	submitter.OnCycle(trace, nil)
	if len(capture.calls) != 6 {
		t.Fatalf("expected the six GitHub submission calls, got %d: %#v", len(capture.calls), capture.calls)
	}
	if !submitter.submitted[trace.ID] {
		t.Fatal("cycle was not recorded as submitted in memory")
	}
	state, err := os.ReadFile(submitter.Config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(state) != trace.ID+"\n" {
		t.Fatalf("state file does not record the submitted cycle: %q", state)
	}

	// A second OnCycle for the same cycle must not submit again.
	submitter.OnCycle(trace, nil)
	if len(capture.calls) != 6 {
		t.Fatalf("expected no additional request for an already-submitted cycle, got %d total", len(capture.calls))
	}
}

func TestContributionAutoSubmitterSkipsIneligibleAndFailedCycles(t *testing.T) {
	dir := t.TempDir()
	capture := &githubCapture{}
	server := newGitHubServer(t, capture, "", "", "base-sha", "https://github.com/imattau/nostrhost-contributions/pull/10", "", 0)
	defer server.Close()
	submitter := newTestAutoSubmitter(t, dir, server.URL)

	incomplete := exportableTrace()
	incomplete.ID = "cycle-incomplete"
	incomplete.FinishedAt = time.Time{}
	incomplete.PlanningCompleted = false
	submitter.OnCycle(incomplete, nil)

	failedCycle := exportableTrace()
	failedCycle.ID = "cycle-failed"
	submitter.OnCycle(failedCycle, errors.New("cycle failed"))

	if len(capture.calls) != 0 {
		t.Fatalf("expected no submission for an ineligible or failed cycle, got %d calls", len(capture.calls))
	}
	if len(submitter.submitted) != 0 {
		t.Fatalf("expected no cycle recorded as submitted, got %#v", submitter.submitted)
	}
}

func TestContributionAutoSubmitterLoadsExistingDedupStateAndNeverReadsTheToken(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.jsonl")
	if err := os.WriteFile(statePath, []byte("cycle-already-done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	submitter, err := NewContributionAutoSubmitter(ContributionFileConfig{
		Enabled: true, DatasetRepo: "owner/repo", TokenPath: filepath.Join(dir, "missing-token"),
		BaseRevision: "main", StatePath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	submitter.apiBaseURL = server.URL
	if !submitter.submitted["cycle-already-done"] {
		t.Fatal("dedup state was not loaded from the existing state file")
	}
	trace := exportableTrace()
	trace.ID = "cycle-already-done"
	submitter.OnCycle(trace, nil)
	if requests != 0 {
		t.Fatalf("already-submitted cycle triggered a request (and would have needed a token that doesn't exist), got %d", requests)
	}
}
