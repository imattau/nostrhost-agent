package agent

import (
	"strings"
)

const redactedValue = "[REDACTED]"

// sanitizeMap creates a detached, safe copy for audit storage. Structured
// operation adapters should still avoid accepting secret values entirely;
// this is defense in depth for host-provided observations and tool results.
func sanitizeMap(input map[string]any, additionallySensitive []string) map[string]any {
	if input == nil {
		return nil
	}
	sensitive := make(map[string]bool, len(additionallySensitive))
	for _, key := range additionallySensitive {
		sensitive[normalizeKey(key)] = true
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		if isSensitiveKey(key) || sensitive[normalizeKey(key)] {
			output[key] = redactedValue
			continue
		}
		output[key] = sanitizeValue(value, sensitive)
	}
	return output
}

func sanitizeValue(value any, sensitive map[string]bool) any {
	switch typed := value.(type) {
	case map[string]any:
		output := make(map[string]any, len(typed))
		for key, nested := range typed {
			if isSensitiveKey(key) || sensitive[normalizeKey(key)] {
				output[key] = redactedValue
			} else {
				output[key] = sanitizeValue(nested, sensitive)
			}
		}
		return output
	case []any:
		output := make([]any, len(typed))
		for i, nested := range typed {
			output[i] = sanitizeValue(nested, sensitive)
		}
		return output
	default:
		return value
	}
}

func safeProposal(proposal Proposal, spec OperationSpec, registered bool) Proposal {
	proposal.Args = sanitizeMap(proposal.Args, spec.SensitiveArgs)
	if !registered {
		// Unknown operations are never executable; retaining their arguments
		// adds little audit value and risks recording arbitrary model output.
		proposal.Args = nil
	}
	return proposal
}

func normalizeKey(value string) string {
	return strings.ToLower(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(value))
}

func isSensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	for _, marker := range []string{"password", "passwd", "token", "secret", "private_key", "api_key", "authorization", "nsec"} {
		if normalized == marker || strings.HasSuffix(normalized, "_"+marker) {
			return true
		}
	}
	return false
}
