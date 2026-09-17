package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

type generatedCatalogDocument struct {
	SchemaVersion int                         `json:"schema_version"`
	Digest        string                      `json:"digest"`
	Operations    []generatedCatalogOperation `json:"operations"`
}

type generatedCatalogOperation struct {
	Name            string          `json:"name"`
	ContractVersion int             `json:"contract_version"`
	Description     string          `json:"description"`
	Scopes          []string        `json:"scopes"`
	Risk            string          `json:"risk"`
	Effect          string          `json:"effect"`
	InputSchema     json.RawMessage `json:"input_schema"`
	ResultSchema    json.RawMessage `json:"result_schema"`
	Approval        struct {
		Minimum string `json:"minimum"`
	} `json:"approval"`
}

var agentOperationProfile = map[string]AutonomyLevel{
	"system.status": Maintain, "service.status": Maintain, "service.history": Maintain,
	"logs.read": Maintain, "diagnosis.run": Maintain, "updates.check": Maintain,
	"backup.create": Maintain, "service.restart": Maintain,
	"app.upgrade": Autonomous, "backup.restore": Autonomous,
	"firewall.open": Autonomous, "firewall.close": Autonomous,
	"nsite.gateway.status": Maintain, "nsite.list": Maintain, "nsite.discover": Maintain,
	"nsite.inspect": Maintain,
	"nsite.resolve": Maintain, "nsite.validate_manifest": Maintain,
	"nsite.reachability": Maintain, "nsite.publish.plan": Maintain,
	"nsite.domain.list": Maintain, "nsite.block.list": Maintain,
}

func generatedAgentRegistry() map[string]OperationSpec {
	var document generatedCatalogDocument
	if err := json.Unmarshal([]byte(generatedCatalogJSON), &document); err != nil || document.SchemaVersion != 2 || document.Digest != GeneratedCatalogDigest {
		panic("invalid generated NostrHost operation catalogue")
	}
	registry := make(map[string]OperationSpec, len(agentOperationProfile))
	for _, operation := range document.Operations {
		autonomy, visible := agentOperationProfile[operation.Name]
		if !visible || len(operation.Scopes) == 0 {
			continue
		}
		risk := RiskLow
		if operation.Effect == "read" {
			risk = RiskRead
		} else if operation.Risk == "medium" {
			risk = RiskElevated
		} else if operation.Risk == "high" {
			risk = RiskDestructive
		}
		scopes := make([]Scope, 0, len(operation.Scopes))
		for _, scope := range operation.Scopes {
			scopes = append(scopes, Scope(scope))
		}
		schema := append([]byte(nil), operation.InputSchema...)
		spec := OperationSpec{
			Name: operation.Name, ContractVersion: operation.ContractVersion,
			Description: operation.Description, Scopes: scopes,
			Risk: risk, AutonomousAt: autonomy,
			RequiresApproval: operation.Approval.Minimum != "none",
			ArgsSchema:       string(schema), ResultSchema: string(operation.ResultSchema),
		}
		registry[operation.Name] = NewOperationSpec(spec, func(args map[string]any) error {
			return validateGeneratedArguments(schema, args)
		})
	}
	if len(registry) != len(agentOperationProfile) {
		panic("generated catalogue is missing an agent-profile operation")
	}
	return registry
}

func validateGeneratedArguments(raw []byte, args map[string]any) error {
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties any                        `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("invalid generated argument schema")
	}
	for _, name := range schema.Required {
		if _, exists := args[name]; !exists {
			return fmt.Errorf("missing required argument %q", name)
		}
	}
	for name, value := range args {
		property, exists := schema.Properties[name]
		if !exists {
			return fmt.Errorf("unexpected argument %q", name)
		}
		if err := validateGeneratedValue(name, property, value); err != nil {
			return err
		}
	}
	return nil
}

func validateGeneratedValue(name string, raw json.RawMessage, value any) error {
	var schema struct {
		Type  string            `json:"type"`
		Enum  []any             `json:"enum"`
		AnyOf []json.RawMessage `json:"anyOf"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("invalid schema for argument %q", name)
	}
	if len(schema.AnyOf) > 0 {
		for _, candidate := range schema.AnyOf {
			if validateGeneratedValue(name, candidate, value) == nil {
				return nil
			}
		}
		return fmt.Errorf("argument %q does not match an allowed type", name)
	}
	if len(schema.Enum) > 0 {
		encoded, _ := json.Marshal(value)
		for _, allowed := range schema.Enum {
			candidate, _ := json.Marshal(allowed)
			if string(encoded) == string(candidate) {
				return nil
			}
		}
		return fmt.Errorf("argument %q is not an allowed value", name)
	}
	if schema.Type == "null" && value == nil {
		return nil
	}
	if schema.Type == "" {
		return nil
	}
	return validateArgument(name, schema.Type, value)
}

// ArgumentValidator checks JSON-shaped arguments against a host-owned schema.
type ArgumentValidator func(map[string]any) error

// NewOperationSpec binds a model-visible JSON schema to a host-side validator.
// The validator is intentionally not serialized with the schema.
func NewOperationSpec(spec OperationSpec, validate ArgumentValidator) OperationSpec {
	if spec.ContractVersion == 0 {
		spec.ContractVersion = 1
	}
	if spec.ResultSchema == "" {
		spec.ResultSchema = `{}`
	}
	spec.validateArgs = validate
	return spec
}

// ValidateRegistry rejects malformed host configuration before the agent
// starts observing or proposing work.
func ValidateRegistry(registry map[string]OperationSpec) error {
	if len(registry) == 0 {
		return fmt.Errorf("operation registry is empty")
	}
	for name, spec := range registry {
		if name == "" || spec.Name != name || len(spec.Scopes) == 0 || spec.ContractVersion < 1 {
			return fmt.Errorf("operation registry key and metadata do not match")
		}
		if spec.Risk > RiskDestructive {
			return fmt.Errorf("operation %q has an invalid risk classification", name)
		}
		if _, ok := autonomyRank[spec.AutonomousAt]; !ok {
			return fmt.Errorf("operation %q has an invalid autonomy threshold", name)
		}
		if !spec.hasArgumentValidator() {
			return fmt.Errorf("operation %q has no argument validator", name)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(spec.ArgsSchema), &schema); err != nil || schema["type"] != "object" {
			return fmt.Errorf("operation %q has an invalid object argument schema", name)
		}
		if !json.Valid([]byte(spec.ResultSchema)) {
			return fmt.Errorf("operation %q has an invalid result schema", name)
		}
	}
	return nil
}

func definedOperation(spec OperationSpec, required, optional map[string]string) OperationSpec {
	properties := make(map[string]map[string]string, len(required)+len(optional))
	for name, kind := range required {
		properties[name] = map[string]string{"type": kind}
	}
	for name, kind := range optional {
		properties[name] = map[string]string{"type": kind}
	}
	requiredNames := make([]string, 0, len(required))
	for name := range required {
		requiredNames = append(requiredNames, name)
	}
	sortStrings(requiredNames)
	schema, _ := json.Marshal(map[string]any{
		"type": "object", "properties": properties,
		"required": requiredNames, "additionalProperties": false,
	})
	spec.ArgsSchema = string(schema)
	return NewOperationSpec(spec, func(args map[string]any) error {
		for name, kind := range required {
			value, exists := args[name]
			if !exists {
				return fmt.Errorf("missing required argument %q", name)
			}
			if err := validateArgument(name, kind, value); err != nil {
				return err
			}
		}
		for name, value := range args {
			kind, ok := required[name]
			if !ok {
				kind, ok = optional[name]
			}
			if !ok {
				return fmt.Errorf("unexpected argument %q", name)
			}
			if _, isRequired := required[name]; !isRequired {
				if err := validateArgument(name, kind, value); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func validateArgument(name, kind string, value any) error {
	valid := false
	switch kind {
	case "string":
		text, ok := value.(string)
		valid = ok && strings.TrimSpace(text) != ""
	case "integer":
		valid = isInteger(value)
	case "number":
		valid = isNumber(value)
	case "boolean":
		_, valid = value.(bool)
	case "object":
		valid = isStringMap(value)
	case "array":
		valid = reflect.ValueOf(value).IsValid() && reflect.ValueOf(value).Kind() == reflect.Slice
	default:
		return fmt.Errorf("operation schema has unsupported type %q", kind)
	}
	if !valid {
		return fmt.Errorf("argument %q must match type %s (strings must be non-empty)", name, kind)
	}
	return nil
}

func isStringMap(value any) bool {
	if _, ok := value.(map[string]any); ok {
		return true
	}
	return false
}

func isInteger(value any) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float64:
		return !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	case float32:
		return !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0) && math.Trunc(float64(number)) == float64(number)
	default:
		return false
	}
}

func isNumber(value any) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return !math.IsNaN(float64(number)) && !math.IsInf(float64(number), 0)
	case float64:
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return false
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
