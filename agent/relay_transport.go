package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const (
	kindOperationRequest = 2200
	kindExecutionResult  = 2204
	defaultResultTimeout = 2 * time.Minute
)

type RelayTransportConfig struct {
	RelayURL         string
	AgentSecretKey   string
	TrustedServerKey string
	TargetPubkey     string
	ResultTimeout    time.Duration
}

type nostrRelayPool interface {
	PublishMany(context.Context, []string, nostr.Event) chan nostr.PublishResult
	SubscribeMany(context.Context, []string, nostr.Filter, ...nostr.SubscriptionOption) chan nostr.RelayEvent
	Close(string)
}

// RelayOperationTransport uses one loopback NostrHost relay. It signs requests
// as the dedicated agent identity, authenticates with NIP-42 when challenged,
// and accepts only signed results from the configured server identity.
type RelayOperationTransport struct {
	pool             nostrRelayPool
	relayURL         string
	agentSecretKey   string
	agentPubkey      string
	trustedServerKey string
	targetPubkey     string
	resultTimeout    time.Duration
}

func NewRelayOperationTransport(cfg RelayTransportConfig) (*RelayOperationTransport, error) {
	if err := validateRelayTransportConfig(cfg); err != nil {
		return nil, err
	}
	agentPubkey, err := nostr.GetPublicKey(cfg.AgentSecretKey)
	if err != nil {
		return nil, errors.New("could not derive agent pubkey from secret key")
	}
	pool := nostr.NewSimplePool(context.Background(), nostr.WithAuthHandler(func(_ context.Context, challenge nostr.RelayEvent) error {
		return challenge.Event.Sign(cfg.AgentSecretKey)
	}))
	transport := newRelayOperationTransport(cfg, agentPubkey, pool)
	return transport, nil
}

func newRelayOperationTransport(cfg RelayTransportConfig, agentPubkey string, pool nostrRelayPool) *RelayOperationTransport {
	timeout := cfg.ResultTimeout
	if timeout <= 0 {
		timeout = defaultResultTimeout
	}
	return &RelayOperationTransport{
		pool: pool, relayURL: nostr.NormalizeURL(cfg.RelayURL),
		agentSecretKey: cfg.AgentSecretKey, agentPubkey: agentPubkey,
		trustedServerKey: strings.ToLower(cfg.TrustedServerKey),
		targetPubkey:     strings.ToLower(cfg.TargetPubkey), resultTimeout: timeout,
	}
}

func (t *RelayOperationTransport) PublishRequest(ctx context.Context, tool string, args map[string]any) (string, error) {
	if t == nil || t.pool == nil {
		return "", errors.New("Nostr relay transport is not initialized")
	}
	if strings.TrimSpace(tool) == "" {
		return "", errors.New("operation name is required")
	}
	content, err := json.Marshal(struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}{Tool: tool, Args: args})
	if err != nil {
		return "", errors.New("operation request is not JSON-compatible")
	}
	if len(content) > maxOperationArgsBytes+1024 {
		return "", errors.New("operation request exceeds the relay payload limit")
	}
	tags := nostr.Tags{}
	if t.targetPubkey != "" {
		tags = append(tags, nostr.Tag{"p", t.targetPubkey})
	}
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: kindOperationRequest, Tags: tags, Content: string(content)}
	if err := event.Sign(t.agentSecretKey); err != nil {
		return "", errors.New("could not sign operation request")
	}
	results := t.pool.PublishMany(ctx, []string{t.relayURL}, event)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result, ok := <-results:
		if !ok {
			return "", errors.New("relay closed without acknowledging operation request")
		}
		if result.Error != nil {
			return "", fmt.Errorf("relay rejected operation request: %w", result.Error)
		}
		return event.ID, nil
	}
}

func (t *RelayOperationTransport) AwaitResult(ctx context.Context, requestID string) (OperationResultEvent, error) {
	if t == nil || t.pool == nil {
		return OperationResultEvent{}, errors.New("Nostr relay transport is not initialized")
	}
	if !validHex64(requestID) {
		return OperationResultEvent{}, errors.New("request id must be a 64-character event id")
	}
	waitCtx, cancel := context.WithTimeout(ctx, t.resultTimeout)
	defer cancel()
	events := t.pool.SubscribeMany(waitCtx, []string{t.relayURL}, nostr.Filter{
		Kinds:   []int{kindExecutionResult},
		Authors: []string{t.trustedServerKey},
		Tags:    nostr.TagMap{"e": {requestID}},
	})
	for {
		select {
		case <-waitCtx.Done():
			return OperationResultEvent{}, waitCtx.Err()
		case received, ok := <-events:
			if !ok {
				return OperationResultEvent{}, errors.New("relay subscription ended before an execution result arrived")
			}
			if received.Event == nil || received.Kind != kindExecutionResult {
				continue
			}
			if err := verifyExecutionResultEvent(received.Event, requestID, t.trustedServerKey); err != nil {
				return OperationResultEvent{}, err
			}
			var body struct {
				OK     *bool          `json:"ok"`
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal([]byte(received.Content), &body); err != nil || body.OK == nil {
				return OperationResultEvent{}, errors.New("execution result has an invalid payload")
			}
			if *body.OK && body.Result == nil {
				return OperationResultEvent{}, errors.New("successful execution result is missing its structured result")
			}
			return OperationResultEvent{RequestID: requestID, Author: received.PubKey, OK: *body.OK, Result: body.Result}, nil
		}
	}
}

func verifyExecutionResultEvent(event *nostr.Event, requestID, trustedServerKey string) error {
	if event.Kind != kindExecutionResult || !strings.EqualFold(event.PubKey, trustedServerKey) {
		return errors.New("execution result has an unexpected kind or author")
	}
	if !event.CheckID() {
		return errors.New("execution result event id is invalid")
	}
	valid, err := event.CheckSignature()
	if err != nil || !valid {
		return errors.New("execution result signature is invalid")
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" && tag[1] == requestID {
			return nil
		}
	}
	return errors.New("execution result does not reference the operation request")
}

func validateRelayTransportConfig(cfg RelayTransportConfig) error {
	if _, err := validateLocalEndpoint(cfg.RelayURL, "ws", "wss"); err != nil {
		return errors.New("control relay URL must use ws/wss and a loopback host")
	}
	if !nostr.IsValid32ByteHex(cfg.AgentSecretKey) {
		return errors.New("agent secret key must be 32-byte hex")
	}
	if !nostr.IsValidPublicKey(cfg.TrustedServerKey) {
		return errors.New("trusted server pubkey must be a valid Nostr public key")
	}
	if cfg.TargetPubkey != "" && !nostr.IsValidPublicKey(cfg.TargetPubkey) {
		return errors.New("target pubkey must be a valid Nostr public key")
	}
	if !nostr.IsValidRelayURL(cfg.RelayURL) {
		return errors.New("control relay URL is invalid")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateLocalEndpoint parses raw and confirms it is a URL that can only
// ever reach a loopback address over one of allowedSchemes, with no
// embedded credentials and no query or fragment component that could carry
// hidden destination or credential material. This is the shared SSRF guard
// for every local (embedding, inference, control relay) endpoint the agent
// is configured to dial; callers must not accept a URL that fails this
// check under any circumstance. On success it returns the parsed URL so
// callers may inspect or rewrite its path.
func validateLocalEndpoint(raw string, allowedSchemes ...string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("endpoint URL is invalid")
	}
	schemeAllowed := false
	for _, scheme := range allowedSchemes {
		if parsed.Scheme == scheme {
			schemeAllowed = true
			break
		}
	}
	if !schemeAllowed || !isLoopbackHost(parsed.Hostname()) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("endpoint must use an allowed scheme on a loopback host without embedded credentials")
	}
	return parsed, nil
}

func (t *RelayOperationTransport) Close() {
	if t != nil && t.pool != nil {
		t.pool.Close("nostrhost agent transport closed")
	}
}
