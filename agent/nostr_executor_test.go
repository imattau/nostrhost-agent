package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeOperationTransport struct {
	requestID string
	request   string
	args      map[string]any
	result    OperationResultEvent
	err       error
	awaits    int
}

func (f *fakeOperationTransport) PublishRequest(_ context.Context, name string, args map[string]any) (string, error) {
	f.request, f.args = name, args
	return f.requestID, f.err
}

func (f *fakeOperationTransport) AwaitResult(_ context.Context, _ string) (OperationResultEvent, error) {
	f.awaits++
	return f.result, f.err
}

func TestNostrOperationExecutorPublishesAndCorrelatesResult(t *testing.T) {
	transport := &fakeOperationTransport{
		requestID: strings.Repeat("a", 64),
		result: OperationResultEvent{
			RequestID: strings.Repeat("a", 64), Author: strings.Repeat("b", 64),
			CatalogDigest: GeneratedCatalogDigest,
			OK:            true,
			Result:        map[string]any{"service": "nginx", "status": "active"},
		},
	}
	executor := NostrOperationExecutor{Transport: transport, ExpectedServerPubkey: strings.Repeat("b", 64)}
	result, err := executor.Execute(context.Background(), DefaultRegistry()["service.restart"], map[string]any{"name": "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if transport.request != "service.restart" || transport.awaits != 1 || result["status"] != "active" {
		t.Fatalf("unexpected operation chain: tool=%q waits=%d result=%#v", transport.request, transport.awaits, result)
	}
}

func TestNostrOperationExecutorRejectsUntrustedOrUncorrelatedResults(t *testing.T) {
	cases := []struct {
		name   string
		result OperationResultEvent
	}{
		{"wrong request", OperationResultEvent{RequestID: strings.Repeat("c", 64), Author: strings.Repeat("b", 64), OK: true}},
		{"wrong author", OperationResultEvent{RequestID: strings.Repeat("a", 64), Author: strings.Repeat("c", 64), OK: true}},
		{"stale catalogue", OperationResultEvent{RequestID: strings.Repeat("a", 64), Author: strings.Repeat("b", 64), CatalogDigest: "sha256:stale", OK: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeOperationTransport{requestID: strings.Repeat("a", 64), result: testCase.result}
			executor := NostrOperationExecutor{Transport: transport, ExpectedServerPubkey: strings.Repeat("b", 64)}
			if _, err := executor.Execute(context.Background(), DefaultRegistry()["service.restart"], map[string]any{"name": "nginx"}); err == nil {
				t.Fatal("untrusted or uncorrelated result accepted")
			}
		})
	}
}

func TestNostrOperationExecutorStopsAtPublishOrValidationFailure(t *testing.T) {
	transport := &fakeOperationTransport{requestID: strings.Repeat("a", 64), err: errors.New("relay down")}
	executor := NostrOperationExecutor{Transport: transport, ExpectedServerPubkey: strings.Repeat("b", 64)}
	if _, err := executor.Execute(context.Background(), DefaultRegistry()["service.restart"], map[string]any{"name": "nginx", "exec": "true"}); err == nil {
		t.Fatal("invalid arguments accepted")
	}
	if transport.request != "" || transport.awaits != 0 {
		t.Fatal("invalid arguments reached relay transport")
	}
	if _, err := executor.Execute(context.Background(), DefaultRegistry()["service.restart"], map[string]any{"name": "nginx"}); err == nil {
		t.Fatal("publish failure ignored")
	}
	if transport.awaits != 0 {
		t.Fatal("waited for result after publish failure")
	}
}

func TestNostrOperationExecutorRequiresConfiguredServerIdentity(t *testing.T) {
	transport := &fakeOperationTransport{}
	executor := NostrOperationExecutor{Transport: transport, ExpectedServerPubkey: "not-a-pubkey"}
	if _, err := executor.Execute(context.Background(), DefaultRegistry()["system.health"], nil); err == nil {
		t.Fatal("missing trusted server identity accepted")
	}
	if transport.request != "" {
		t.Fatal("request published before trusted server identity validation")
	}
}

func TestNostrOperationExecutorUsesRequestBoundHostApprovalChain(t *testing.T) {
	executor := NostrOperationExecutor{
		Transport: &fakeOperationTransport{}, ExpectedServerPubkey: strings.Repeat("b", 64),
	}
	if !executor.UsesAuthoritativeApprovalChain() {
		t.Fatal("configured Nostr executor did not expose its host approval chain")
	}
	executor.Transport = nil
	if executor.UsesAuthoritativeApprovalChain() {
		t.Fatal("unconfigured Nostr executor claimed an approval chain")
	}
}

func TestNostrOperationExecutorNeverPublishesUnregisteredOperation(t *testing.T) {
	transport := &fakeOperationTransport{}
	executor := NostrOperationExecutor{Transport: transport, ExpectedServerPubkey: strings.Repeat("b", 64)}
	custom := NewOperationSpec(OperationSpec{Name: "shell.exec", Scopes: []Scope{Scope("server.read")}}, func(map[string]any) error { return nil })
	if _, err := executor.Execute(context.Background(), custom, map[string]any{"command": "id"}); err == nil {
		t.Fatal("unregistered operation was accepted")
	}
	if transport.request != "" {
		t.Fatal("unregistered operation reached the relay")
	}
}
