package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VerificationRule defines a fresh read-only operation and the value expected
// after a particular write operation. CheckArgs maps query argument names to
// proposal argument names; ConstantArgs supplies fixed query arguments.
type VerificationRule struct {
	Operation      string
	CheckOperation string
	CheckArgs      map[string]string
	ConstantArgs   map[string]any
	ResultPath     string
	Expected       any
}

type NostrOperationVerifier struct {
	Executor OperationExecutor
	Registry map[string]OperationSpec
	Rules    map[string]VerificationRule
}

func NewNostrOperationVerifier(executor OperationExecutor, registry map[string]OperationSpec, rules []VerificationRule) (*NostrOperationVerifier, error) {
	if executor == nil {
		return nil, errors.New("Nostr operation verifier requires an executor")
	}
	if registry == nil {
		registry = DefaultRegistry()
	}
	if err := ValidateRegistry(registry); err != nil {
		return nil, fmt.Errorf("invalid operation registry: %w", err)
	}
	if len(rules) == 0 {
		return nil, errors.New("Nostr operation verifier requires at least one verification rule")
	}
	registered := make(map[string]OperationSpec, len(registry))
	for name, spec := range registry {
		registered[name] = spec
	}
	configured := make(map[string]VerificationRule, len(rules))
	for _, rule := range rules {
		operation, exists := registered[rule.Operation]
		if !exists || operation.Risk == RiskRead {
			return nil, fmt.Errorf("verification rule names an unknown or read-only action %q", rule.Operation)
		}
		query, exists := registered[rule.CheckOperation]
		if !exists || query.Risk != RiskRead || query.RequiresApproval {
			return nil, fmt.Errorf("verification rule %q must use a registered read-only check", rule.Operation)
		}
		if _, duplicate := configured[rule.Operation]; duplicate {
			return nil, fmt.Errorf("duplicate verification rule for %q", rule.Operation)
		}
		if strings.TrimSpace(rule.ResultPath) == "" {
			return nil, fmt.Errorf("verification rule %q requires a result path", rule.Operation)
		}
		queryProperties, err := schemaProperties(query.ArgsSchema)
		if err != nil {
			return nil, fmt.Errorf("verification rule %q has an invalid check schema", rule.Operation)
		}
		operationProperties, err := schemaProperties(operation.ArgsSchema)
		if err != nil {
			return nil, fmt.Errorf("verification rule %q has an invalid action schema", rule.Operation)
		}
		constants, err := cloneJSONMap(rule.ConstantArgs)
		if err != nil {
			return nil, fmt.Errorf("verification rule %q has invalid constant arguments", rule.Operation)
		}
		expected, err := cloneJSONValue(rule.Expected)
		if err != nil {
			return nil, fmt.Errorf("verification rule %q has invalid expected value", rule.Operation)
		}
		args := make(map[string]any, len(constants)+len(rule.CheckArgs))
		for key, value := range constants {
			if _, exists := queryProperties[key]; !exists {
				return nil, fmt.Errorf("verification rule %q has unknown check argument %q", rule.Operation, key)
			}
			args[key] = value
		}
		for queryArg, proposalArg := range rule.CheckArgs {
			if queryArg == "" || proposalArg == "" {
				return nil, fmt.Errorf("verification rule %q has an empty argument mapping", rule.Operation)
			}
			if _, duplicate := args[queryArg]; duplicate {
				return nil, fmt.Errorf("verification rule %q maps argument %q more than once", rule.Operation, queryArg)
			}
			if _, exists := queryProperties[queryArg]; !exists {
				return nil, fmt.Errorf("verification rule %q has unknown check argument %q", rule.Operation, queryArg)
			}
			if _, exists := operationProperties[proposalArg]; !exists {
				return nil, fmt.Errorf("verification rule %q maps from unknown proposal argument %q", rule.Operation, proposalArg)
			}
			args[queryArg] = "verification-placeholder"
		}
		for key := range args {
			if _, exists := queryProperties[key]; !exists {
				return nil, fmt.Errorf("verification rule %q has unknown check argument %q", rule.Operation, key)
			}
		}
		if len(rule.CheckArgs) == 0 {
			if err := query.ValidateArgs(args); err != nil {
				return nil, fmt.Errorf("verification rule %q has invalid check arguments", rule.Operation)
			}
		}
		rule.CheckArgs = cloneStringMap(rule.CheckArgs)
		rule.ConstantArgs = constants
		rule.Expected = expected
		configured[rule.Operation] = rule
	}
	return &NostrOperationVerifier{Executor: executor, Registry: registered, Rules: configured}, nil
}

func schemaProperties(schema string) (map[string]json.RawMessage, error) {
	var parsed struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &parsed); err != nil || parsed.Type != "object" {
		return nil, errors.New("schema must describe an object")
	}
	return parsed.Properties, nil
}

func (v *NostrOperationVerifier) Verify(ctx context.Context, _ map[string]any, proposal Proposal, _ map[string]any) (bool, error) {
	if v == nil || v.Executor == nil {
		return false, errors.New("Nostr operation verifier is not initialized")
	}
	rule, exists := v.Rules[proposal.Operation]
	if !exists {
		return false, fmt.Errorf("no verification rule is configured for %q", proposal.Operation)
	}
	args := make(map[string]any, len(rule.ConstantArgs)+len(rule.CheckArgs))
	for key, value := range rule.ConstantArgs {
		args[key] = value
	}
	for queryArg, proposalArg := range rule.CheckArgs {
		value, exists := proposal.Args[proposalArg]
		if !exists {
			return false, fmt.Errorf("proposal is missing verification input %q", proposalArg)
		}
		args[queryArg] = value
	}
	check := v.Registry[rule.CheckOperation]
	if err := check.ValidateArgs(args); err != nil {
		return false, errors.New("verification check arguments failed schema validation")
	}
	result, err := v.Executor.Execute(ctx, check, args)
	if err != nil {
		return false, fmt.Errorf("run fresh verification check: %w", err)
	}
	actual, exists := valueAtPath(result, rule.ResultPath)
	if !exists {
		return false, nil
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		return false, errors.New("verification result is not JSON-compatible")
	}
	expectedJSON, err := json.Marshal(rule.Expected)
	if err != nil {
		return false, errors.New("configured verification value is not JSON-compatible")
	}
	return string(actualJSON) == string(expectedJSON), nil
}

func valueAtPath(value any, path string) (any, bool) {
	current := value
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func cloneJSONValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxOperationArgsBytes {
		return nil, errors.New("value is not JSON-compatible or exceeds the size limit")
	}
	var cloned any
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
