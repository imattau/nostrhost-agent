package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
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
		validate, err := compileArgumentValidator(schema)
		if err != nil {
			panic(fmt.Sprintf("operation %q has an invalid argument schema: %v", operation.Name, err))
		}
		spec := OperationSpec{
			Name: operation.Name, ContractVersion: operation.ContractVersion,
			Description: operation.Description, Scopes: scopes,
			Risk: risk, AutonomousAt: autonomy,
			RequiresApproval: operation.Approval.Minimum != "none",
			ArgsSchema:       string(schema), ResultSchema: string(operation.ResultSchema),
		}
		registry[operation.Name] = NewOperationSpec(spec, validate)
	}
	if len(registry) != len(agentOperationProfile) {
		panic("generated catalogue is missing an agent-profile operation")
	}
	return registry
}

// compileArgumentValidator compiles a JSON Schema once and returns a validator
// that checks a decoded-args map against it. JSON Schema handles structural
// conformance (types, enum, anyOf, required, additionalProperties, bounds,
// items); project-specific semantic rules that are not expressible in the
// generated schemas are applied on top (see non-empty string handling below).
func compileArgumentValidator(raw []byte) (ArgumentValidator, error) {
	compiler := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid generated argument schema")
	}
	if err := compiler.AddResource("args.json", doc); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile("args.json")
	if err != nil {
		return nil, err
	}
	return func(args map[string]any) error {
		// Project-specific semantic rule preserved outside JSON Schema: a
		// supplied string argument must be non-empty (the generated schemas
		// do not carry a minLength keyword).
		for name, value := range args {
			if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
				return fmt.Errorf("argument %q must be non-empty", name)
			}
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("operation arguments are not JSON-compatible")
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		if err := compiled.Validate(instance); err != nil {
			return fmt.Errorf("argument validation failed: %v", err)
		}
		return nil
	}, nil
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