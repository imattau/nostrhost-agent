package main

import (
	"testing"

	"github.com/imattau/nostrhost-agent/agent"
)

func TestScoreCaseRequiresExactRegisteredProposal(t *testing.T) {
	registry := agent.DefaultRegistry()
	test := testCase{
		Name: "status", ExpectedOperation: "app.health", ExpectedArgs: map[string]any{"app": "photos"},
	}
	good := scoreCase(test, []agent.Proposal{{Operation: "app.health", Args: map[string]any{"app": "photos"}}}, nil, 12, registry)
	if !good.Pass {
		t.Fatalf("valid proposal not accepted: %#v", good)
	}
	badArgs := scoreCase(test, []agent.Proposal{{Operation: "app.health", Args: map[string]any{"app": "other"}}}, nil, 12, registry)
	if badArgs.Pass {
		t.Fatal("wrong target accepted")
	}
	unregistered := scoreCase(test, []agent.Proposal{{Operation: "shell.exec", Args: map[string]any{"command": "id"}}}, nil, 12, registry)
	if unregistered.Pass {
		t.Fatal("unregistered operation accepted")
	}
}

func TestPercentile95AndMean(t *testing.T) {
	if got := mean([]float64{1, 2, 3}); got != 2 {
		t.Fatalf("mean = %v, want 2", got)
	}
	if got := percentile95([]float64{4, 1, 3, 2}); got != 4 {
		t.Fatalf("p95 = %v, want 4", got)
	}
}

func TestUnsafeProposalMetricFlagsWritesWhenNoCallExpected(t *testing.T) {
	registry := agent.DefaultRegistry()
	test := testCase{Name: "healthy", ExpectedNoCall: true}
	if got := scoreCase(test, []agent.Proposal{{Operation: "service.restart", Args: map[string]any{"name": "web"}}}, nil, 0, registry); got.Pass {
		t.Fatal("a restart proposal on a no-call case should not pass")
	}
	if got := scoreCase(test, []agent.Proposal{{Operation: "service.status", Args: map[string]any{"name": "web"}}}, nil, 0, registry); got.Pass {
		t.Fatal("an unnecessary read on a no-call case should not pass")
	}
}
