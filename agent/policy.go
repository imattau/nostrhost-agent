// Package agent defines the unprivileged planner's typed operation boundary.
package agent

import (
	"encoding/json"
	"fmt"
)

const maxOperationArgsBytes = 64 * 1024

// AutonomyLevel controls whether an otherwise-capable proposal may execute
// without owner approval. Scope grants remain an independent hard limit.
type AutonomyLevel string

const (
	Observe    AutonomyLevel = "observe"
	Assist     AutonomyLevel = "assist"
	Maintain   AutonomyLevel = "maintain"
	Autonomous AutonomyLevel = "autonomous"
)

// Scope is a native NostrHost authorization scope copied directly from the
// operation catalogue.
type Scope string

// Risk expresses the policy class of a registered operation. It is metadata
// controlled by the host, never supplied by the model.
type Risk uint8

const (
	RiskRead Risk = iota
	RiskLow
	RiskElevated
	RiskDestructive
)

// OperationSpec is the host-owned description of a typed tool. ArgsSchema is
// descriptive only; the operation adapter must validate concrete arguments.
type OperationSpec struct {
	Name             string
	ContractVersion  int
	Description      string
	Scopes           []Scope
	Risk             Risk
	AutonomousAt     AutonomyLevel
	RequiresApproval bool
	ArgsSchema       string
	ResultSchema     string
	SensitiveArgs    []string
	validateArgs     ArgumentValidator
}

// Proposal is a model-authored request to invoke a registered operation.
type Proposal struct {
	Operation string         `json:"operation"`
	Args      map[string]any `json:"args"`
}

// Decision is a policy result. AllowExecution means only that policy permits
// an attempt; the executor still performs scope and argument checks.
type Decision string

const (
	DecisionObserveOnly  Decision = "observe_only"
	DecisionProposalOnly Decision = "proposal_only"
	DecisionApproval     Decision = "approval_required"
	DecisionAllow        Decision = "allow"
	DecisionDeny         Decision = "deny"
)

type Policy struct {
	Level  AutonomyLevel
	Scopes map[Scope]bool
}

type PolicyResult struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason"`
}

var autonomyRank = map[AutonomyLevel]int{
	Observe:    0,
	Assist:     1,
	Maintain:   2,
	Autonomous: 3,
}

func (p Policy) Evaluate(spec OperationSpec) PolicyResult {
	if _, ok := autonomyRank[p.Level]; !ok {
		return PolicyResult{Decision: DecisionDeny, Reason: fmt.Sprintf("unknown autonomy level %q", p.Level)}
	}
	if spec.Name == "" || len(spec.Scopes) == 0 {
		return PolicyResult{Decision: DecisionDeny, Reason: "operation registry entry is incomplete"}
	}
	if spec.Risk > RiskDestructive {
		return PolicyResult{Decision: DecisionDeny, Reason: "operation has invalid risk classification"}
	}
	for _, scope := range spec.Scopes {
		if !p.Scopes[scope] {
			return PolicyResult{Decision: DecisionDeny, Reason: fmt.Sprintf("missing scope %q", scope)}
		}
	}
	if p.Level == Observe {
		return PolicyResult{Decision: DecisionObserveOnly, Reason: "observe mode never executes proposals"}
	}
	if p.Level == Assist {
		return PolicyResult{Decision: DecisionProposalOnly, Reason: "assist mode proposes plans but never executes them"}
	}
	if spec.Risk >= RiskElevated || spec.RequiresApproval {
		return PolicyResult{Decision: DecisionApproval, Reason: "operation requires owner approval"}
	}
	threshold, ok := autonomyRank[spec.AutonomousAt]
	if !ok {
		return PolicyResult{Decision: DecisionDeny, Reason: "operation has invalid autonomy threshold"}
	}
	if autonomyRank[p.Level] < threshold {
		return PolicyResult{Decision: DecisionApproval, Reason: fmt.Sprintf("%s autonomy is required", spec.AutonomousAt)}
	}
	return PolicyResult{Decision: DecisionAllow, Reason: "scope and maintenance policy permit execution"}
}

// ValidateArgs enforces the host-owned argument schema before any approval
// request or executor call. The executor must still decode into its typed
// operation arguments and apply its own validation.
func (s OperationSpec) ValidateArgs(args map[string]any) error {
	if s.validateArgs == nil {
		return fmt.Errorf("operation %q has no argument validator", s.Name)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("operation arguments are not JSON-compatible")
	}
	if len(encoded) > maxOperationArgsBytes {
		return fmt.Errorf("operation arguments exceed the %d-byte limit", maxOperationArgsBytes)
	}
	return s.validateArgs(args)
}

func (s OperationSpec) hasArgumentValidator() bool { return s.validateArgs != nil }

// DefaultRegistry is generated from the native catalogue and filtered through
// a hand-reviewed model-visible profile. The control plane remains
// authoritative and validates the same operation contract again.
func DefaultRegistry() map[string]OperationSpec {
	return generatedAgentRegistry()
}

func EvaluateProposal(policy Policy, registry map[string]OperationSpec, proposal Proposal) PolicyResult {
	spec, ok := registry[proposal.Operation]
	if !ok {
		return PolicyResult{Decision: DecisionDeny, Reason: fmt.Sprintf("unknown operation %q", proposal.Operation)}
	}
	return policy.Evaluate(spec)
}
