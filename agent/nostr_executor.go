package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OperationResultEvent is the verified projection of one kind-2204 event.
// RequestID comes from its e-tag; Author is the event's verified pubkey.
type OperationResultEvent struct {
	RequestID string
	Author    string
	OK        bool
	Result    map[string]any
}

// OperationTransport publishes a signed kind-2200 request and waits for its
// correlated kind-2204 result. Implementations must verify Nostr event ids and
// signatures, use the e-tag for correlation, and honor the caller's context.
// PublishRequest must sign as the configured agent identity; it never accepts
// shell text or an unregistered operation name from callers outside this pkg.
type OperationTransport interface {
	PublishRequest(context.Context, string, map[string]any) (string, error)
	AwaitResult(context.Context, string) (OperationResultEvent, error)
}

// NostrOperationExecutor adapts the cycle runner to NostrHost's signed
// operation chain. The authoritative operation daemon still evaluates its own
// capabilities, approvals, and typed handler before changing machine state.
type NostrOperationExecutor struct {
	Transport            OperationTransport
	ExpectedServerPubkey string
	Registry             map[string]OperationSpec
}

// NewControlPlaneExecutor wires the loopback relay transport to the host's
// operation registry and trusted server identity.
func NewControlPlaneExecutor(cfg RelayTransportConfig, registry map[string]OperationSpec) (*NostrOperationExecutor, error) {
	transport, err := NewRelayOperationTransport(cfg)
	if err != nil {
		return nil, err
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	if err := ValidateRegistry(registry); err != nil {
		transport.Close()
		return nil, fmt.Errorf("invalid operation registry: %w", err)
	}
	registered := make(map[string]OperationSpec, len(registry))
	for name, spec := range registry {
		registered[name] = spec
	}
	return &NostrOperationExecutor{
		Transport: transport, ExpectedServerPubkey: cfg.TrustedServerKey,
		Registry: registered,
	}, nil
}

func (e *NostrOperationExecutor) Close() {
	if e == nil || e.Transport == nil {
		return
	}
	if closer, ok := e.Transport.(interface{ Close() }); ok {
		closer.Close()
	}
}

func (e NostrOperationExecutor) Execute(ctx context.Context, spec OperationSpec, args map[string]any) (map[string]any, error) {
	if e.Transport == nil {
		return nil, errors.New("operation transport is not configured")
	}
	if !validHex64(e.ExpectedServerPubkey) {
		return nil, errors.New("expected control-plane server pubkey must be 64-character hex")
	}
	registry := e.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}
	registered, ok := registry[spec.Name]
	if !ok {
		return nil, errors.New("operation is not present in the host registry")
	}
	if err := registered.ValidateArgs(args); err != nil {
		return nil, errors.New("operation arguments failed local schema validation")
	}
	requestID, err := e.Transport.PublishRequest(ctx, registered.Name, args)
	if err != nil {
		return nil, fmt.Errorf("publish operation request: %w", err)
	}
	if !validHex64(requestID) {
		return nil, errors.New("operation transport returned an invalid request id")
	}
	result, err := e.Transport.AwaitResult(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("await operation result: %w", err)
	}
	if !strings.EqualFold(result.RequestID, requestID) {
		return nil, errors.New("operation result is not correlated to the published request")
	}
	if !strings.EqualFold(result.Author, e.ExpectedServerPubkey) {
		return nil, errors.New("operation result was not authored by the configured control-plane server")
	}
	if !result.OK {
		return nil, errors.New("NostrHost operation was rejected or failed")
	}
	return result.Result, nil
}

func validHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
