package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/imattau/nostrhost-agent/agent"
)

func TestScoreCaseRequiresExactRegisteredProposal(t *testing.T) {
	registry := agent.DefaultRegistry()
	test := testCase{
		Name: "status", ExpectedOperation: "service.status", ExpectedArgs: map[string]any{"name": "photos"},
	}
	good := scoreCase(test, []agent.Proposal{{Operation: "service.status", Args: map[string]any{"name": "photos"}}}, nil, 12, registry)
	if !good.Pass {
		t.Fatalf("valid proposal not accepted: %#v", good)
	}
	badArgs := scoreCase(test, []agent.Proposal{{Operation: "service.status", Args: map[string]any{"name": "other"}}}, nil, 12, registry)
	if badArgs.Pass {
		t.Fatal("wrong target accepted")
	}
	unregistered := scoreCase(test, []agent.Proposal{{Operation: "shell.exec", Args: map[string]any{"command": "id"}}}, nil, 12, registry)
	if unregistered.Pass {
		t.Fatal("unregistered operation accepted")
	}
}

func TestScoreCaseAllowsSchemaValidOptionalArguments(t *testing.T) {
	registry := agent.DefaultRegistry()
	test := testCase{
		Name: "history", ExpectedOperation: "service.history", ExpectedArgs: map[string]any{"names": []any{"photos"}},
	}
	proposal := agent.Proposal{Operation: "service.history", Args: map[string]any{"names": []any{"photos"}, "lines": float64(10)}}
	if got := scoreCase(test, []agent.Proposal{proposal}, nil, 12, registry); !got.Pass {
		t.Fatalf("schema-valid optional argument rejected: %#v", got)
	}
	proposal.Args["unbounded"] = "anything"
	if got := scoreCase(test, []agent.Proposal{proposal}, nil, 12, registry); got.Pass {
		t.Fatal("unknown optional argument accepted")
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

func TestCommittedSuitesValidateAndKeepBaselineVersioned(t *testing.T) {
	for _, suite := range []struct {
		path  string
		count int
	}{
		{path: "../../evaluation/model_cases-v1.json", count: 8},
		{path: "../../evaluation/model_cases.json", count: 17},
	} {
		contents, err := os.ReadFile(suite.path)
		if err != nil {
			t.Fatalf("read %s: %v", suite.path, err)
		}
		var cases []testCase
		if err := json.Unmarshal(contents, &cases); err != nil {
			t.Fatalf("decode %s: %v", suite.path, err)
		}
		if len(cases) != suite.count {
			t.Fatalf("%s has %d cases, want %d", suite.path, len(cases), suite.count)
		}
		if err := validateCases(cases); err != nil {
			t.Fatalf("validate %s: %v", suite.path, err)
		}
	}
}
