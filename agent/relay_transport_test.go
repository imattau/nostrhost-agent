package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

type fakeNostrPool struct {
	urls      []string
	published nostr.Event
	filter    nostr.Filter
	response  *nostr.Event
	closed    bool
}

func (f *fakeNostrPool) PublishMany(_ context.Context, urls []string, event nostr.Event) chan nostr.PublishResult {
	f.urls = append([]string(nil), urls...)
	f.published = event
	results := make(chan nostr.PublishResult, 1)
	results <- nostr.PublishResult{RelayURL: urls[0]}
	close(results)
	return results
}

func (f *fakeNostrPool) SubscribeMany(_ context.Context, urls []string, filter nostr.Filter, _ ...nostr.SubscriptionOption) chan nostr.RelayEvent {
	f.urls = append([]string(nil), urls...)
	f.filter = filter
	events := make(chan nostr.RelayEvent, 1)
	if f.response != nil {
		events <- nostr.RelayEvent{Event: f.response}
	}
	close(events)
	return events
}

func (f *fakeNostrPool) Close(string) { f.closed = true }

func TestRelayOperationTransportSignsAndPublishesAgentRequest(t *testing.T) {
	secret := strings.Repeat("1", 64)
	agentPubkey, err := nostr.GetPublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	serverSecret := strings.Repeat("2", 64)
	serverPubkey, err := nostr.GetPublicKey(serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	pool := &fakeNostrPool{}
	transport := newRelayOperationTransport(RelayTransportConfig{
		RelayURL: "ws://127.0.0.1:4848", AgentSecretKey: secret,
		TrustedServerKey: serverPubkey, TargetPubkey: strings.Repeat("3", 64),
	}, agentPubkey, pool)
	requestID, err := transport.PublishRequest(context.Background(), "service.restart", map[string]any{"name": "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if requestID != pool.published.ID || requestID == "" || pool.published.Kind != kindOperationRequest || pool.published.PubKey != agentPubkey {
		t.Fatalf("published request identity/kind mismatch: %#v", pool.published)
	}
	if !pool.published.CheckID() {
		t.Fatal("published event id is invalid")
	}
	valid, err := pool.published.CheckSignature()
	if err != nil || !valid {
		t.Fatalf("published event signature invalid: valid=%v err=%v", valid, err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(pool.published.Content), &body); err != nil || body["tool"] != "service.restart" {
		t.Fatalf("unexpected operation request content: %s", pool.published.Content)
	}
	if pool.published.Tags.FindWithValue("p", strings.Repeat("3", 64)) == nil {
		t.Fatalf("target pubkey tag missing: %#v", pool.published.Tags)
	}
}

func TestRelayOperationTransportVerifiesCorrelatedServerResult(t *testing.T) {
	requestID := strings.Repeat("a", 64)
	serverSecret := strings.Repeat("2", 64)
	serverPubkey, err := nostr.GetPublicKey(serverSecret)
	if err != nil {
		t.Fatal(err)
	}
	event := nostr.Event{
		CreatedAt: nostr.Now(), Kind: kindExecutionResult,
		Tags:    nostr.Tags{nostr.Tag{"e", requestID}},
		Content: `{"ok":true,"result":{"status":"active"}}`,
	}
	if err := event.Sign(serverSecret); err != nil {
		t.Fatal(err)
	}
	pool := &fakeNostrPool{response: &event}
	transport := newRelayOperationTransport(RelayTransportConfig{
		RelayURL: "ws://localhost:4848", AgentSecretKey: strings.Repeat("1", 64),
		TrustedServerKey: serverPubkey,
	}, strings.Repeat("4", 64), pool)
	result, err := transport.AwaitResult(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequestID != requestID || result.Author != serverPubkey || result.Result["status"] != "active" {
		t.Fatalf("unexpected result projection: %#v", result)
	}
	if len(pool.filter.Kinds) != 1 || pool.filter.Kinds[0] != kindExecutionResult || pool.filter.Tags["e"][0] != requestID || pool.filter.Authors[0] != serverPubkey {
		t.Fatalf("result subscription was not scoped: %#v", pool.filter)
	}
}

func TestRelayOperationTransportRejectsInvalidResultSignature(t *testing.T) {
	requestID := strings.Repeat("a", 64)
	serverSecret := strings.Repeat("2", 64)
	serverPubkey, _ := nostr.GetPublicKey(serverSecret)
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: kindExecutionResult, Tags: nostr.Tags{nostr.Tag{"e", requestID}}, Content: `{"ok":true}`}
	_ = event.Sign(serverSecret)
	event.Content = `{"ok":false}` // invalidate the signed body
	transport := newRelayOperationTransport(RelayTransportConfig{
		RelayURL: "ws://127.0.0.1:4848", AgentSecretKey: strings.Repeat("1", 64), TrustedServerKey: serverPubkey,
	}, strings.Repeat("4", 64), &fakeNostrPool{response: &event})
	if _, err := transport.AwaitResult(context.Background(), requestID); err == nil {
		t.Fatal("invalidly signed result was accepted")
	}
}

func TestRelayOperationTransportConfigRequiresLoopbackAndKeys(t *testing.T) {
	secret := strings.Repeat("1", 64)
	serverPubkey, _ := nostr.GetPublicKey(strings.Repeat("2", 64))
	cfg := RelayTransportConfig{RelayURL: "ws://127.0.0.1:4848", AgentSecretKey: secret, TrustedServerKey: serverPubkey}
	if err := validateRelayTransportConfig(cfg); err != nil {
		t.Fatalf("valid local config rejected: %v", err)
	}
	cfg.RelayURL = "wss://relay.example.com"
	if err := validateRelayTransportConfig(cfg); err == nil {
		t.Fatal("remote relay URL accepted")
	}
	cfg.RelayURL = "ws://user:pass@127.0.0.1:4848/?token=secret"
	if err := validateRelayTransportConfig(cfg); err == nil {
		t.Fatal("relay URL with embedded credentials or query accepted")
	}
	cfg.RelayURL = "ws://127.0.0.1:4848"
	cfg.TargetPubkey = "invalid"
	if err := validateRelayTransportConfig(cfg); err == nil {
		t.Fatal("invalid target pubkey accepted")
	}
}

func TestRelayOperationTransportCloseClosesPool(t *testing.T) {
	pool := &fakeNostrPool{}
	transport := newRelayOperationTransport(RelayTransportConfig{}, strings.Repeat("1", 64), pool)
	transport.Close()
	if !pool.closed {
		t.Fatal("transport did not close its relay pool")
	}
}

func TestRelayOperationTransportResultWaitHonorsContext(t *testing.T) {
	serverPubkey, _ := nostr.GetPublicKey(strings.Repeat("2", 64))
	transport := newRelayOperationTransport(RelayTransportConfig{
		RelayURL: "ws://127.0.0.1:4848", AgentSecretKey: strings.Repeat("1", 64),
		TrustedServerKey: serverPubkey, ResultTimeout: time.Second,
	}, strings.Repeat("4", 64), &fakeNostrPool{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.AwaitResult(ctx, strings.Repeat("a", 64)); err == nil {
		t.Fatal("canceled context did not stop result wait")
	}
}
