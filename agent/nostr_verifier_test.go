package agent

import (
	"context"
	"errors"
	"testing"
)

func TestNostrOperationVerifierRunsFreshReadAndChecksExpectedValue(t *testing.T) {
	executor := &recordingOperationExecutor{results: map[string]map[string]any{
		"service.status": {"service": map[string]any{"status": "active"}},
	}}
	verifier, err := NewNostrOperationVerifier(executor, nil, []VerificationRule{{
		Operation: "service.restart", CheckOperation: "service.status",
		CheckArgs: map[string]string{"name": "name"}, ResultPath: "service.status", Expected: "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(context.Background(), nil, Proposal{
		Operation: "service.restart", Args: map[string]any{"name": "web"},
	}, nil)
	if err != nil || !verified {
		t.Fatalf("fresh read did not verify the service: verified=%v err=%v", verified, err)
	}
	if len(executor.operations) != 1 || executor.operations[0] != "service.status" || executor.args[0]["name"] != "web" {
		t.Fatalf("unexpected verification read: ops=%#v args=%#v", executor.operations, executor.args)
	}
}

func TestNostrOperationVerifierDistinguishesMismatchAndReadFailure(t *testing.T) {
	executor := &recordingOperationExecutor{results: map[string]map[string]any{"service.status": {"status": "inactive"}}}
	verifier, err := NewNostrOperationVerifier(executor, nil, []VerificationRule{{
		Operation: "service.restart", CheckOperation: "service.status",
		CheckArgs: map[string]string{"name": "name"}, ResultPath: "status", Expected: "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{Operation: "service.restart", Args: map[string]any{"name": "web"}}
	verified, err := verifier.Verify(context.Background(), nil, proposal, nil)
	if err != nil || verified {
		t.Fatalf("unexpected state was treated as verified: verified=%v err=%v", verified, err)
	}
	executor.failures = map[string]error{"service.status": errors.New("read failed")}
	if _, err := verifier.Verify(context.Background(), nil, proposal, nil); err == nil {
		t.Fatal("failed fresh read was not reported as a verification error")
	}
}

func TestNostrOperationVerifierRejectsUnsafeOrMalformedRules(t *testing.T) {
	executor := &recordingOperationExecutor{}
	if _, err := NewNostrOperationVerifier(executor, nil, []VerificationRule{{
		Operation: "service.restart", CheckOperation: "service.restart", CheckArgs: map[string]string{"name": "name"}, ResultPath: "status", Expected: "active",
	}}); err == nil {
		t.Fatal("write operation accepted as a verification read")
	}
	if _, err := NewNostrOperationVerifier(executor, nil, []VerificationRule{{
		Operation: "service.restart", CheckOperation: "service.status", CheckArgs: map[string]string{"unknown": "name"}, ResultPath: "status", Expected: "active",
	}}); err == nil {
		t.Fatal("unknown verification argument accepted")
	}
}
