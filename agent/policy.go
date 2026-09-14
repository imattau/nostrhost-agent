// Package agent defines the unprivileged planner's typed operation boundary.
package agent

import (
	"encoding/json"
	"fmt"
)

const maxOperationArgsBytes = 64 * 1024

// AutonomyLevel controls whether an otherwise-capable proposal may execute
// without owner approval. Capability grants remain an independent hard limit.
type AutonomyLevel string

const (
	Observe    AutonomyLevel = "observe"
	Assist     AutonomyLevel = "assist"
	Maintain   AutonomyLevel = "maintain"
	Autonomous AutonomyLevel = "autonomous"
)

// Capability names match the NostrHost policy vocabulary where one exists.
type Capability string

const (
	SystemRead     Capability = "system.read"
	HealthRead     Capability = "health.read"
	LogsRead       Capability = "logs.read"
	DiagnosisRun   Capability = "diagnosis.run"
	BackupCreate   Capability = "backup.create"
	ServiceRestart Capability = "service.restart"
	StateDiff      Capability = "state.diff"
	PackageUpdate  Capability = "package.update"
	AppRestore     Capability = "app.restore"
	FirewallWrite  Capability = "firewall.write"
	NsitesRead     Capability = "nsites.read"
)

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
	Description      string
	Capability       Capability
	Risk             Risk
	AutonomousAt     AutonomyLevel
	RequiresApproval bool
	ArgsSchema       string
	SensitiveArgs    []string
	validateArgs     ArgumentValidator
}

// Proposal is a model-authored request to invoke a registered operation.
type Proposal struct {
	Operation string         `json:"operation"`
	Args      map[string]any `json:"args"`
}

// Decision is a policy result. AllowExecution means only that policy permits
// an attempt; the executor still performs capability and argument checks.
type Decision string

const (
	DecisionObserveOnly  Decision = "observe_only"
	DecisionProposalOnly Decision = "proposal_only"
	DecisionApproval     Decision = "approval_required"
	DecisionAllow        Decision = "allow"
	DecisionDeny         Decision = "deny"
)

type Policy struct {
	Level        AutonomyLevel
	Capabilities map[Capability]bool
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
	if spec.Name == "" || spec.Capability == "" {
		return PolicyResult{Decision: DecisionDeny, Reason: "operation registry entry is incomplete"}
	}
	if spec.Risk > RiskDestructive {
		return PolicyResult{Decision: DecisionDeny, Reason: "operation has invalid risk classification"}
	}
	if !p.Capabilities[spec.Capability] {
		return PolicyResult{Decision: DecisionDeny, Reason: fmt.Sprintf("missing capability %q", spec.Capability)}
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
	return PolicyResult{Decision: DecisionAllow, Reason: "capability and maintenance policy permit execution"}
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

// DefaultRegistry is intentionally small and conservative. Operation names
// are stable API identifiers; no operation accepts arbitrary commands.
func DefaultRegistry() map[string]OperationSpec {
	specs := []OperationSpec{
		definedOperation(OperationSpec{Name: "system.health", Description: "Summarized host health and current health-check failures.", Capability: HealthRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "service.status", Description: "Read status for all services or one named service.", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, map[string]string{"name": "string"}),
		definedOperation(OperationSpec{Name: "app.health", Description: "Read health status for one installed application.", Capability: HealthRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"app": "string"}, nil),
		definedOperation(OperationSpec{Name: "app.logs", Description: "Read a bounded recent log excerpt for one application.", Capability: LogsRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"app": "string"}, map[string]string{"lines": "integer"}),
		definedOperation(OperationSpec{Name: "disk.status", Description: "Read filesystem usage and available space.", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "diagnosis.run", Description: "Run one registered diagnostic for the selected target.", Capability: DiagnosisRun, Risk: RiskLow, AutonomousAt: Maintain}, nil, map[string]string{"target": "string"}),
		definedOperation(OperationSpec{Name: "backup.create", Description: "Create a point-in-time backup for the host or one application.", Capability: BackupCreate, Risk: RiskLow, AutonomousAt: Maintain}, nil, map[string]string{"app": "string"}),
		definedOperation(OperationSpec{Name: "service.restart", Description: "Restart one known service by name.", Capability: ServiceRestart, Risk: RiskLow, AutonomousAt: Maintain}, map[string]string{"name": "string"}, nil),
		definedOperation(OperationSpec{Name: "state.diff", Description: "Read a bounded semantic state diff.", Capability: StateDiff, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "package.updates", Description: "List pending system and application updates without installing them.", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "package.upgrade", Description: "Upgrade one named application; owner approval is required.", Capability: PackageUpdate, Risk: RiskElevated, AutonomousAt: Autonomous, RequiresApproval: true}, map[string]string{"app": "string"}, nil),
		definedOperation(OperationSpec{Name: "app.restore", Description: "Restore one application snapshot; owner approval is required.", Capability: AppRestore, Risk: RiskDestructive, AutonomousAt: Autonomous, RequiresApproval: true}, map[string]string{"app": "string", "snapshot": "string"}, nil),
		definedOperation(OperationSpec{Name: "firewall.change", Description: "Change a firewall rule; owner approval is required.", Capability: FirewallWrite, Risk: RiskDestructive, AutonomousAt: Autonomous, RequiresApproval: true}, map[string]string{"action": "string", "port": "integer"}, map[string]string{"protocol": "string"}),
		// NIP-5A nsites read surface (Phase 3b): read-only tools are available
		// for observation; every nsite.* write is deliberately absent from the
		// registry, so any proposal for one is denied (unknown operation) in
		// every autonomy level including autonomous.
		definedOperation(OperationSpec{Name: "nsite.gateway.status", Description: "Read the nsite gateway status (enabled, mode, domain, health).", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "nsite.list", Description: "List registered nsite sites and the gateway mode.", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
		definedOperation(OperationSpec{Name: "nsite.inspect", Description: "Read one registered nsite site record.", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"pubkey": "string"}, map[string]string{"d": "string"}),
		definedOperation(OperationSpec{Name: "nsite.resolve", Description: "Fetch a site manifest from public relays (read only, bounded).", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"pubkey": "string"}, map[string]string{"label": "string", "d": "string"}),
		definedOperation(OperationSpec{Name: "nsite.validate_manifest", Description: "Validate a candidate manifest event; no network.", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"event": "string"}, nil),
		definedOperation(OperationSpec{Name: "nsite.reachability", Description: "Probe relay/server reachability (bounded).", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, map[string]string{"relays": "string", "servers": "string"}),
		definedOperation(OperationSpec{Name: "nsite.publish.plan", Description: "Build an unsigned manifest + plan digest from an inventory or draft site.", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, map[string]string{"pubkey": "string"}, map[string]string{"kind": "integer", "d": "string", "site": "string"}),
		definedOperation(OperationSpec{Name: "nsite.domain.list", Description: "List attached custom domains (Phase 4, read only).", Capability: NsitesRead, Risk: RiskRead, AutonomousAt: Maintain}, nil, nil),
	}
	registry := make(map[string]OperationSpec, len(specs))
	for _, spec := range specs {
		registry[spec.Name] = spec
	}
	return registry
}

func EvaluateProposal(policy Policy, registry map[string]OperationSpec, proposal Proposal) PolicyResult {
	spec, ok := registry[proposal.Operation]
	if !ok {
		return PolicyResult{Decision: DecisionDeny, Reason: fmt.Sprintf("unknown operation %q", proposal.Operation)}
	}
	return policy.Evaluate(spec)
}
