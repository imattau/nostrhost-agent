package agent

import "testing"

func TestPolicySeparatesCapabilityFromAutonomy(t *testing.T) {
	registry := DefaultRegistry()
	maintain := Policy{Level: Maintain, Scopes: map[Scope]bool{Scope("services.restart"): true}}

	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "service.restart"}).Decision; got != DecisionApproval {
		t.Fatalf("service restart decision = %q, want approval", got)
	}
	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "firewall.open"}).Decision; got != DecisionDeny {
		t.Fatalf("firewall change decision = %q, want deny", got)
	}
	maintain.Scopes[Scope("firewall.write")] = true
	if got := EvaluateProposal(maintain, registry, Proposal{Operation: "firewall.open"}).Decision; got != DecisionApproval {
		t.Fatalf("firewall change with scope decision = %q, want approval", got)
	}
}

func TestPolicyAutonomyModesAndUnknownOperations(t *testing.T) {
	registry := DefaultRegistry()
	policy := Policy{Level: Observe, Scopes: map[Scope]bool{"services.read": true}}
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "service.status"}).Decision; got != DecisionObserveOnly {
		t.Fatalf("observe decision = %q, want observe_only", got)
	}
	policy.Level = Assist
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "service.status"}).Decision; got != DecisionProposalOnly {
		t.Fatalf("assist decision = %q, want proposal_only", got)
	}
	if got := EvaluateProposal(policy, registry, Proposal{Operation: "shell.exec"}).Decision; got != DecisionDeny {
		t.Fatalf("unknown operation decision = %q, want deny", got)
	}
}

func TestPolicyRejectsInvalidRegistryThreshold(t *testing.T) {
	policy := Policy{Level: Autonomous, Scopes: map[Scope]bool{"x": true}}
	got := policy.Evaluate(OperationSpec{Name: "x", Scopes: []Scope{"x"}, AutonomousAt: "future"})
	if got.Decision != DecisionDeny {
		t.Fatalf("decision = %q, want deny", got.Decision)
	}
}

func TestElevatedRiskAlwaysRequiresApproval(t *testing.T) {
	policy := Policy{Level: Autonomous, Scopes: map[Scope]bool{Scope("apps.upgrade"): true}}
	spec := OperationSpec{Name: "package.upgrade", Scopes: []Scope{Scope("apps.upgrade")}, Risk: RiskElevated, AutonomousAt: Autonomous}
	if got := policy.Evaluate(spec).Decision; got != DecisionApproval {
		t.Fatalf("elevated operation decision = %q, want approval_required", got)
	}
}

func TestAuditSanitizesProposalObservationsAndResults(t *testing.T) {
	proposal := Proposal{Operation: "service.restart", Args: map[string]any{
		"name": "web", "api-token": "sensitive", "nested": map[string]any{"nsec": "private"},
	}}
	safe := safeProposal(proposal, OperationSpec{SensitiveArgs: []string{"name"}}, true)
	if safe.Args["api-token"] != redactedValue || safe.Args["name"] != redactedValue {
		t.Fatalf("proposal arguments were not sanitized: %#v", safe.Args)
	}
	if got := safe.Args["nested"].(map[string]any)["nsec"]; got != redactedValue {
		t.Fatalf("nested secret was not sanitized: %#v", safe.Args)
	}
	if proposal.Args["api-token"] != "sensitive" {
		t.Fatal("sanitizer mutated the planner-owned proposal")
	}
	if got := sanitizeMap(map[string]any{"access_token": "x", "state": "active"}, nil); got["access_token"] != redactedValue || got["state"] != "active" {
		t.Fatalf("structured result was not sanitized: %#v", got)
	}
}

func TestAuditSanitizesPreviouslyMissedSecretKeyNames(t *testing.T) {
	for _, key := range []string{
		"secret_key", "signing_key", "encryption_key", "credentials",
		"passwordHash", "client_secret", "db_password", "API_KEY",
	} {
		got := sanitizeMap(map[string]any{key: "sensitive", "pubkey": "keep"}, nil)
		if got[key] != redactedValue {
			t.Fatalf("key %q was not redacted: %#v", key, got)
		}
		if got["pubkey"] != "keep" {
			t.Fatalf("benign key was over-redacted: %#v", got)
		}
	}
}
