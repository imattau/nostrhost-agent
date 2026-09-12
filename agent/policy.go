// Package agent defines the unprivileged planner's typed operation boundary.
package agent

import "fmt"

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
	Capability       Capability
	Risk             Risk
	AutonomousAt     AutonomyLevel
	RequiresApproval bool
	ArgsSchema       string
	SensitiveArgs    []string
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

// DefaultRegistry is intentionally small and conservative. Operation names
// are stable API identifiers; no operation accepts arbitrary commands.
func DefaultRegistry() map[string]OperationSpec {
	specs := []OperationSpec{
		{Name: "system.health", Capability: HealthRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "service.status", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "app.health", Capability: HealthRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "app.logs", Capability: LogsRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "disk.status", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "diagnosis.run", Capability: DiagnosisRun, Risk: RiskLow, AutonomousAt: Maintain},
		{Name: "backup.create", Capability: BackupCreate, Risk: RiskLow, AutonomousAt: Maintain},
		{Name: "service.restart", Capability: ServiceRestart, Risk: RiskLow, AutonomousAt: Maintain},
		{Name: "state.diff", Capability: StateDiff, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "package.updates", Capability: SystemRead, Risk: RiskRead, AutonomousAt: Maintain},
		{Name: "package.upgrade", Capability: PackageUpdate, Risk: RiskElevated, AutonomousAt: Autonomous, RequiresApproval: true},
		{Name: "app.restore", Capability: AppRestore, Risk: RiskDestructive, AutonomousAt: Autonomous, RequiresApproval: true},
		{Name: "firewall.change", Capability: FirewallWrite, Risk: RiskDestructive, AutonomousAt: Autonomous, RequiresApproval: true},
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
