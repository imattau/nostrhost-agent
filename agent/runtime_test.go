package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func testRuntimeConfig(t *testing.T) RuntimeConfig {
	t.Helper()
	serverPubkey, err := nostr.GetPublicKey(strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeConfig{
		Relay: RelayTransportConfig{
			RelayURL: "ws://127.0.0.1:4848", AgentSecretKey: strings.Repeat("1", 64), TrustedServerKey: serverPubkey,
		},
		Policy:             Policy{Level: Observe, Capabilities: map[Capability]bool{HealthRead: true}},
		ObservationQueries: []ObservationQuery{{Operation: "system.health"}},
		AuditPath:          filepath.Join(t.TempDir(), "audit.jsonl"),
	}
}

func TestResidentRuntimeOwnsResourcesAndRunsOnce(t *testing.T) {
	runtime, err := NewResidentRuntime(testRuntimeConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("resident runtime was run twice")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("close after run failed: %v", err)
	}
}

func TestResidentRuntimeCanCloseBeforeStarting(t *testing.T) {
	runtime, err := NewResidentRuntime(testRuntimeConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("closed runtime started")
	}
}

func TestResidentRuntimePreflightsObservationCapabilities(t *testing.T) {
	cfg := testRuntimeConfig(t)
	cfg.Policy.Capabilities = map[Capability]bool{}
	if _, err := NewResidentRuntime(cfg); err == nil {
		t.Fatal("observation query bypassed local capability configuration")
	}
}

func TestResidentRuntimeSkipsInferenceInObserveMode(t *testing.T) {
	cfg := testRuntimeConfig(t)
	cfg.Inference = LLMPlannerConfig{BaseURL: "https://remote.example", Model: "not-used"}
	runtime, err := NewResidentRuntime(cfg)
	if err != nil {
		t.Fatalf("observe mode initialized an unused inference endpoint: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResidentRuntimeBuildsFreshReadVerifierForMaintenance(t *testing.T) {
	cfg := testRuntimeConfig(t)
	cfg.Policy = Policy{Level: Maintain, Capabilities: map[Capability]bool{
		HealthRead: true, ServiceRestart: true, SystemRead: true,
	}}
	cfg.Inference = LLMPlannerConfig{BaseURL: "http://127.0.0.1:8080", Model: "local-model"}
	cfg.VerificationRules = []VerificationRule{{
		Operation: "service.restart", CheckOperation: "service.status",
		CheckArgs: map[string]string{"name": "name"}, ResultPath: "status", Expected: "active",
	}}
	runtime, err := NewResidentRuntime(cfg)
	if err != nil {
		t.Fatalf("maintenance runtime did not assemble configured verifier: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}
