package agent

import "testing"

func TestPolicySeparatesCapabilityFromAutonomy(t *testing.T) {
	registry := DefaultRegistry()
	maintain := Policy{Level: Maintain, Capabilities: map[Capability]bool{ServiceRestart: true}}

	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "service.restart"}).Decision; got != DecisionAllow {
		t.Fatalf("service restart decision = %q, want allow", got)
	}
	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "firewall.change"}).Decision; got != DecisionDeny {
		t.Fatalf("firewall change decision = %q, want deny", got)
	}
	maintain.Capabilities[FirewallWrite] = true
	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "firewall.change"}).Decision; got != DecisionApproval {
		t.Fatalf("firewall change with capability decision = %q, want approval", got)
	}
}

func TestPolicyAutonomyModesAndUnknownOperations(t *testing.T) {
	registry := DefaultRegistry()
	policy := Policy{Level: Observe, Capabilities: map[Capability]bool{HealthRead: true}}
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "app.health"}).Decision; got != DecisionObserveOnly {
		t.Fatalf("observe decision = %q, want observe_only", got)
	}
	policy.Level = Assist
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "app.health"}).Decision; got != DecisionProposalOnly {
		t.Fatalf("assist decision = %q, want proposal_only", got)
	}
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "shell.exec"}).Decision; got != DecisionDeny {
		t.Fatalf("unknown operation decision = %q, want deny", got)
	}
}

func TestPolicyRejectsInvalidRegistryThreshold(t *testing.T) {
	policy := Policy{Level: Autonomous, Capabilities: map[Capability]bool{"x": true}}
	got := policy.Evaluate(OperationSpec{Name: "x", Capability: "x", AutonomousAt: "future"})
	if got.Decision != DecisionDeny {
		t.Fatalf("decision = %q, want deny", got.Decision)
	}
}
