package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// ArgumentValidator checks JSON-shaped arguments against a host-owned schema.
type ArgumentValidator func(map[string]any) error

// NewOperationSpec binds a model-visible JSON schema to a host-side validator.
// The validator is intentionally not serialized with the schema.
func NewOperationSpec(spec OperationSpec, validate ArgumentValidator) OperationSpec {
	spec.validateArgs = validate
	return spec
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
