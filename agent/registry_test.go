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
		"service.restart": {Name: "service.restart", Capability: ServiceRestart, Risk: RiskLow, AutonomousAt: Maintain, ArgsSchema: `{"type":"object"}`},
	}
	if err := ValidateRegistry(bad); err == nil {
		t.Fatal("operation without host-side argument validator accepted")
	}
}
