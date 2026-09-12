package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ObservationQuery declares one host-registered, read-only operation used to
// build the structured observation map. TargetArg, when set, receives the
// current cycle target.
type ObservationQuery struct {
	Operation string
	Args      map[string]any
	TargetArg string
}

// NostrOperationObserver builds read models from selected operation results.
// Each query still passes through the host operation registry and relay chain.
type NostrOperationObserver struct {
	Executor OperationExecutor
	Registry map[string]OperationSpec
	Queries  []ObservationQuery
}

func NewNostrOperationObserver(executor OperationExecutor, registry map[string]OperationSpec, queries []ObservationQuery) (*NostrOperationObserver, error) {
	if executor == nil {
		return nil, errors.New("Nostr operation observer requires an executor")
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	if err := ValidateRegistry(registry); err != nil {
		return nil, fmt.Errorf("invalid operation registry: %w", err)
	}
	if len(queries) == 0 {
		return nil, errors.New("Nostr operation observer requires at least one query")
	}
	registered := make(map[string]OperationSpec, len(registry))
	for name, spec := range registry {
		registered[name] = spec
	}
	seen := make(map[string]bool, len(queries))
	configured := make([]ObservationQuery, 0, len(queries))
	for _, query := range queries {
		spec, exists := registered[query.Operation]
		if !exists || spec.Risk != RiskRead || spec.RequiresApproval {
			return nil, fmt.Errorf("observation query %q must be a registered read-only operation", query.Operation)
		}
		if seen[query.Operation] {
			return nil, fmt.Errorf("duplicate observation query %q", query.Operation)
		}
		seen[query.Operation] = true
		args, err := cloneJSONMap(query.Args)
		if err != nil {
			return nil, fmt.Errorf("observation query %q has invalid arguments", query.Operation)
		}
		if query.TargetArg != "" {
			if _, exists := args[query.TargetArg]; exists {
				return nil, fmt.Errorf("observation query %q target argument is already configured", query.Operation)
			}
			args[query.TargetArg] = "observation-target"
		}
		if err := spec.ValidateArgs(args); err != nil {
			return nil, fmt.Errorf("observation query %q has invalid arguments", query.Operation)
		}
		query.Args = args
		configured = append(configured, query)
	}
	return &NostrOperationObserver{Executor: executor, Registry: registered, Queries: configured}, nil
}

func (o *NostrOperationObserver) Observe(ctx context.Context, _ string, target string) (map[string]any, error) {
	if o == nil || o.Executor == nil || len(o.Queries) == 0 {
		return nil, errors.New("Nostr operation observer is not initialized")
	}
	observations := make(map[string]any, len(o.Queries))
	for _, query := range o.Queries {
		if err := ctx.Err(); err != nil {
			return observations, err
		}
		args := make(map[string]any, len(query.Args)+1)
		for key, value := range query.Args {
			args[key] = value
		}
		if query.TargetArg != "" {
			args[query.TargetArg] = target
		}
		spec := o.Registry[query.Operation]
		if err := spec.ValidateArgs(args); err != nil {
			observations[query.Operation] = map[string]any{"ok": false, "error": "invalid_observation_arguments"}
			continue
		}
		result, err := o.Executor.Execute(ctx, spec, args)
		if err != nil {
			if ctx.Err() != nil {
				return observations, ctx.Err()
			}
			observations[query.Operation] = map[string]any{"ok": false, "error": "read_failed"}
			continue
		}
		observations[query.Operation] = map[string]any{"ok": true, "data": result}
	}
	return observations, nil
}

func cloneJSONMap(value map[string]any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxOperationArgsBytes {
		return nil, errors.New("arguments are not JSON-compatible or exceed the size limit")
	}
	var cloned map[string]any
	if err := json.Unmarshal(encoded, &cloned); err != nil || cloned == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return cloned, nil
}
