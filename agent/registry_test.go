package agent

import "testing"

func TestValidateRegistryRejectsMalformedHostConfiguration(t *testing.T) {
	if err := ValidateRegistry(DefaultRegistry()); err != nil {
		t.Fatalf("default registry invalid: %v", err)
	}
	if err := ValidateRegistry(nil); err == nil {
		t.Fatal("empty registry accepted")
	}
	bad := map[string]OperationSpec{
		"service.restart": {Name: "service.restart", Scopes: []Scope{Scope("services.restart")}, Risk: RiskLow, AutonomousAt: Maintain, ArgsSchema: `{"type":"object"}`},
	}
	if err := ValidateRegistry(bad); err == nil {
		t.Fatal("operation without host-side argument validator accepted")
	}
}

func TestNsiteReadsRegisteredAndWritesAbsent(t *testing.T) {
	// Phase 3b: the read surface is available for observation; every nsite.*
	// write is absent from the registry so a proposal for one is denied
	// (unknown operation) in every autonomy level including autonomous.
	registry := DefaultRegistry()
	reads := []string{
		"nsite.gateway.status",
		"nsite.list",
		"nsite.inspect",
		"nsite.resolve",
		"nsite.validate_manifest",
		"nsite.reachability",
		"nsite.publish.plan",
		"nsite.domain.list",
	}
	for _, name := range reads {
		spec, ok := registry[name]
		if !ok {
			t.Fatalf("nsite read %q is not registered", name)
		}
		if spec.Risk != RiskRead {
			t.Fatalf("nsite read %q has risk %d, want RiskRead", name, spec.Risk)
		}
		if len(spec.Scopes) != 1 || spec.Scopes[0] != Scope("nsites.read") {
			t.Fatalf("nsite read %q has scopes %q, want nsites.read", name, spec.Scopes)
		}
	}
	writes := []string{
		"nsite.publish",
		"nsite.snapshot",
		"nsite.mirror",
		"nsite.register",
		"nsite.unregister",
		"nsite.gateway.enable",
		"nsite.gateway.disable",
		"nsite.gateway.configure",
		"nsite.domain.attach",
		"nsite.domain.detach",
	}
	for _, name := range writes {
		if _, ok := registry[name]; ok {
			t.Fatalf("nsite write %q must NOT be in the agent registry", name)
		}
	}
	// an explicit proposal for a write must be denied even at autonomous level
	policy := Policy{Level: Autonomous, Scopes: map[Scope]bool{Scope("nsites.read"): true}}
	for _, name := range writes {
		result := EvaluateProposal(policy, registry, Proposal{Operation: name})
		if result.Decision != DecisionDeny {
			t.Fatalf("nsite write %q not denied at autonomous: %s", name, result.Decision)
		}
	}
}
