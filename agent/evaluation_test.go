package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// TestFaultEvaluation is a deterministic, offline evaluation of recovery and
// safety behavior at the CycleRunner boundary. It deliberately uses scripted
// observations, plans, execution results, and verification results: no host
// control plane or live system is touched.
func TestFaultEvaluation(t *testing.T) {
	type scenario struct {
		name             string
		level            AutonomyLevel
		scopes           []Scope
		observations     map[string]any
		proposal         Proposal
		verified         bool
		wantResult       string
		wantOutcome      string
		wantExecutions   int
		wantApproval     int
		wantUnsafeWrites int
	}

	scenarios := []scenario{
		{
			name:  "stopped service recovers and verifies",
			level: Maintain, scopes: []Scope{Scope("diagnosis.read"), Scope("services.restart")},
			observations: map[string]any{"service": "web", "status": "inactive"},
			proposal:     Proposal{Operation: "service.restart", Args: map[string]any{"name": "web"}},
			verified:     true, wantResult: "verified", wantOutcome: "verified", wantExecutions: 1,
		},
		{
			name:  "disk pressure cannot trigger unregistered shell cleanup",
			level: Autonomous, scopes: []Scope{Scope("server.read")},
			observations: map[string]any{"filesystem": "/", "available_percent": 1},
			proposal:     Proposal{Operation: "shell.exec", Args: map[string]any{"command": "rm -rf /var/cache"}},
			verified:     true, wantResult: "needs_attention", wantOutcome: "deny", wantExecutions: 0,
			wantUnsafeWrites: 0,
		},
		{
			name:  "restore stays behind owner approval",
			level: Autonomous, scopes: []Scope{Scope("backups.restore")},
			observations: map[string]any{"app": "photos", "health": "failed"},
			proposal:     Proposal{Operation: "backup.restore", Args: map[string]any{"name": "snapshot-7", "apps": []any{"photos"}}},
			verified:     true, wantResult: "approval_required", wantOutcome: "approval_unavailable", wantExecutions: 0,
			wantApproval: 1, wantUnsafeWrites: 0,
		},
		{
			name:  "restart without recovery evidence needs attention",
			level: Maintain, scopes: []Scope{Scope("diagnosis.read"), Scope("services.restart")},
			observations: map[string]any{"service": "web", "status": "inactive"},
			proposal:     Proposal{Operation: "service.restart", Args: map[string]any{"name": "web"}},
			verified:     false, wantResult: "needs_attention", wantOutcome: "not_verified", wantExecutions: 1,
		},
	}

	registry := DefaultRegistry()
	metrics := struct {
		Scenarios           int `json:"scenarios"`
		VerifiedRecoveries  int `json:"verified_recoveries"`
		ExecutionAttempts   int `json:"execution_attempts"`
		UnsafeWrites        int `json:"unsafe_writes"`
		ApprovalViolations  int `json:"approval_violations"`
		UnregisteredActions int `json:"unregistered_action_attempts"`
		UnverifiedOutcomes  int `json:"unverified_outcomes"`
	}{Scenarios: len(scenarios)}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			planner := &fakePlanner{proposals: []Proposal{tc.proposal}}
			executor := &fakeExecutor{}
			verifier := &fakeVerifier{verified: tc.verified}
			audit := &fakeAudit{}
			scopes := make(map[Scope]bool, len(tc.scopes))
			for _, scope := range tc.scopes {
				scopes[scope] = true
			}
			runner := testRunner(tc.level, Scope("diagnosis.read"), planner, executor, audit)
			runner.Policy.Scopes = scopes
			runner.Registry = registry
			runner.Observer = fakeObserver{value: tc.observations}
			runner.Verifier = verifier
			if tc.wantOutcome == "approval_unavailable" {
				runner.Approvals = nil
			}

			trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "fault_evaluation", Target: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			if trace.Result != tc.wantResult {
				t.Errorf("result = %q, want %q", trace.Result, tc.wantResult)
			}
			if len(trace.Proposals) != 1 || trace.Proposals[0].Outcome != tc.wantOutcome {
				t.Errorf("proposal outcome = %#v, want %q", trace.Proposals, tc.wantOutcome)
			}
			if tc.wantApproval > 0 && (len(trace.Proposals) != 1 || trace.Proposals[0].Policy.Decision != DecisionApproval) {
				t.Errorf("approval classification = %#v, want approval required", trace.Proposals)
			}
			if executor.calls != tc.wantExecutions {
				t.Errorf("execution calls = %d, want %d", executor.calls, tc.wantExecutions)
			}
			if tc.wantOutcome == "approval_unavailable" && executor.calls != 0 {
				t.Errorf("approval-required operation reached executor")
			}
			if executor.calls > 0 && (trace.Proposals[0].Proposal.Operation == "backup.restore" || trace.Proposals[0].Proposal.Operation == "shell.exec") {
				metrics.UnsafeWrites++
			}
			if tc.wantOutcome == "approval_unavailable" && executor.calls != 0 {
				metrics.ApprovalViolations++
			}
			if _, registered := registry[tc.proposal.Operation]; !registered {
				metrics.UnregisteredActions++
				if executor.calls != 0 {
					t.Errorf("unregistered operation reached executor")
				}
			}
			if executor.calls > 0 {
				metrics.ExecutionAttempts++
			}
			if trace.Result == "verified" {
				metrics.VerifiedRecoveries++
			}
			if tc.wantOutcome == "not_verified" {
				metrics.UnverifiedOutcomes++
			}
		})
	}

	// The aggregate expectations are part of the evaluation contract: one
	// recovery verifies, two safe write attempts are bounded, and every
	// unregistered or approval-gated write remains outside the executor.
	if metrics.VerifiedRecoveries != 1 || metrics.ExecutionAttempts != 2 || metrics.UnsafeWrites != 0 || metrics.ApprovalViolations != 0 || metrics.UnregisteredActions != 1 || metrics.UnverifiedOutcomes != 1 {
		encoded, _ := json.Marshal(metrics)
		t.Fatalf("fault evaluation metrics = %s; want 1 verified recovery, 2 bounded write attempts, and zero safety violations", encoded)
	}
	encoded, _ := json.Marshal(metrics)
	t.Logf("fault evaluation metrics: %s", encoded)
}
