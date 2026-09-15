package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// A cycle executes at most one proposal so every write is followed by fresh
// observations on the next cycle instead of acting on a stale multi-step plan.
const hardMaxProposals = 1

// Observer returns a structured, redacted read model. Implementations must not
// expose arbitrary filesystem contents or secrets to the planner or audit log.
type Observer interface {
	Observe(context.Context, string, string) (map[string]any, error)
}

// Planner is untrusted. Its proposals are treated as data and checked against
// the host registry and policy before any operation reaches the executor.
type Planner interface {
	Plan(context.Context, PlanningInput) ([]Proposal, error)
}

type PlanningInput struct {
	Trigger      string
	Target       string
	Observations map[string]any
	Knowledge    []KnowledgeMatch
	Operations   []OperationSpec
}

// OperationExecutor dispatches only registered operations. Implementations
// must decode args into operation-specific typed structs, reject unknown or
// invalid fields, return redacted structured results, and call only the native
// NostrHost control plane.
type OperationExecutor interface {
	Execute(context.Context, OperationSpec, map[string]any) (map[string]any, error)
}

// ApprovalChainExecutor identifies an executor whose authoritative control
// plane waits for and verifies signed owner approval after it receives the
// request. This is needed when approval events reference the request event ID.
type ApprovalChainExecutor interface {
	OperationExecutor
	UsesAuthoritativeApprovalChain() bool
}

// ApprovalGate must verify signed owner approval bound to this proposal and
// current policy context. The NostrHost executor can instead delegate the
// request-bound approval step to the control plane after creating the request.
type ApprovalGate interface {
	Approve(context.Context, Proposal, PolicyResult) (bool, error)
}

type Verifier interface {
	Verify(context.Context, map[string]any, Proposal, map[string]any) (bool, error)
}

// AuditSink.Save performs an idempotent upsert keyed by trace ID. The runner
// saves before observations, after each stage, and before every execution.
type AuditSink interface {
	Save(context.Context, CycleTrace) error
}

type CycleRequest struct {
	Trigger string
	Target  string
}

type CycleRunner struct {
	Policy       Policy
	Registry     map[string]OperationSpec
	Observer     Observer
	Planner      Planner
	Retriever    Retriever
	Executor     OperationExecutor
	Approvals    ApprovalGate
	Verifier     Verifier
	Audit        AuditSink
	MaxProposals int
	Now          func() time.Time
	NewID        func() (string, error)
}

func (r CycleRunner) Run(ctx context.Context, request CycleRequest) (CycleTrace, error) {
	if r.Observer == nil || r.Audit == nil {
		return CycleTrace{}, errors.New("cycle runner requires observer and audit sink")
	}
	if request.Trigger == "" {
		return CycleTrace{}, errors.New("cycle trigger is required")
	}
	if err := ValidateRegistry(r.Registry); err != nil {
		return CycleTrace{}, fmt.Errorf("invalid operation registry: %w", err)
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	newID := r.NewID
	if newID == nil {
		newID = randomID
	}
	id, err := newID()
	if err != nil {
		return CycleTrace{}, fmt.Errorf("create cycle id: %w", err)
	}
	trace := CycleTrace{ID: id, StartedAt: now().UTC(), Trigger: request.Trigger, Target: request.Target, Result: "running"}
	if err := r.Audit.Save(ctx, trace); err != nil {
		return trace, fmt.Errorf("persist initial audit record: %w", err)
	}

	observations, err := r.Observer.Observe(ctx, request.Trigger, request.Target)
	if err != nil {
		return r.finish(ctx, trace, now, "observation_failed", "could not collect structured observations", err)
	}
	trace.Observations = sanitizeMap(observations, nil)
	trace.Capabilities = sortedCapabilities(r.Policy.Capabilities)
	if err := r.save(ctx, trace); err != nil {
		return trace, fmt.Errorf("persist observations: %w", err)
	}
	if r.Policy.Level == Observe {
		return r.finish(ctx, trace, now, "observed", "observe mode does not create execution proposals", nil)
	}
	if r.Planner == nil {
		return r.finish(ctx, trace, now, "planner_unavailable", "no planner is configured", errors.New("planner is required outside observe mode"))
	}
	if r.Executor == nil && (r.Policy.Level == Maintain || r.Policy.Level == Autonomous) {
		return r.finish(ctx, trace, now, "executor_unavailable", "no operation executor is configured", errors.New("executor is required for execution modes"))
	}
	var knowledge []KnowledgeMatch
	if r.Retriever != nil {
		query, marshalErr := json.Marshal(struct {
			Trigger      string         `json:"trigger"`
			Target       string         `json:"target,omitempty"`
			Observations map[string]any `json:"observations"`
		}{request.Trigger, request.Target, trace.Observations})
		if marshalErr != nil {
			return r.finish(ctx, trace, now, "retrieval_failed", "could not construct a local retrieval query", marshalErr)
		}
		knowledge, err = r.Retriever.Retrieve(ctx, string(query), maxKnowledgeResults)
		if err != nil {
			return r.finish(ctx, trace, now, "retrieval_failed", "could not retrieve local operating context", err)
		}
		for _, match := range knowledge {
			trace.Knowledge = append(trace.Knowledge, KnowledgeCitation{
				ID: match.ID, Source: match.Source, Hash: match.Hash, Score: match.Score,
			})
		}
		if err := r.save(ctx, trace); err != nil {
			return trace, fmt.Errorf("persist retrieval citations: %w", err)
		}
	}
	operations := r.availableOperations()
	trace.AvailableOperations = snapshotOperations(operations)
	proposals, err := r.Planner.Plan(ctx, PlanningInput{
		Trigger: request.Trigger, Target: request.Target, Observations: trace.Observations,
		Knowledge: knowledge, Operations: operations,
	})
	if err != nil {
		return r.finish(ctx, trace, now, "planning_failed", "planner could not produce a plan", err)
	}
	trace.PlanningCompleted = true
	if err := r.save(ctx, trace); err != nil {
		return trace, fmt.Errorf("persist completed planner decision: %w", err)
	}
	limit := r.MaxProposals
	if limit <= 0 {
		limit = hardMaxProposals
	}
	if limit > hardMaxProposals {
		limit = hardMaxProposals
	}
	if len(proposals) > limit {
		proposals = proposals[:limit]
		trace.Result = "proposal_limit_reached"
		trace.Resolution = "planner output was truncated to the configured proposal limit"
	}

	for _, proposal := range proposals {
		if err := ctx.Err(); err != nil {
			return r.finish(ctx, trace, now, "interrupted", "cycle context ended before all proposals were handled", err)
		}
		result := EvaluateProposal(r.Policy, r.Registry, proposal)
		spec, registered := r.Registry[proposal.Operation]
		outcome := string(result.Decision)
		if registered && result.Decision != DecisionDeny {
			if err := spec.ValidateArgs(proposal.Args); err != nil {
				result = PolicyResult{Decision: DecisionDeny, Reason: "arguments do not match the registered operation schema"}
				outcome = "invalid_arguments"
			}
		}
		record := ProposalRecord{Proposal: safeProposal(proposal, spec, registered), Policy: result, Outcome: outcome}
		if outcome == "invalid_arguments" {
			record.Proposal.Args = nil
		}
		trace.Proposals = append(trace.Proposals, record)
		index := len(trace.Proposals) - 1
		if err := r.save(ctx, trace); err != nil {
			return trace, fmt.Errorf("persist proposal and policy decision: %w", err)
		}
		if result.Decision == DecisionDeny || result.Decision == DecisionObserveOnly {
			continue
		}
		if result.Decision == DecisionProposalOnly {
			continue
		}
		if result.Decision == DecisionApproval {
			if r.Approvals == nil {
				delegated, ok := r.Executor.(ApprovalChainExecutor)
				if !ok || !delegated.UsesAuthoritativeApprovalChain() {
					if err := r.recordOutcome(ctx, trace, index, "approval_unavailable", "persist approval requirement"); err != nil {
						return trace, err
					}
					continue
				}
				trace.Proposals[index].Outcome = "approval_delegated"
				if err := r.save(ctx, trace); err != nil {
					return trace, fmt.Errorf("persist delegated approval requirement: %w", err)
				}
			} else {
				approved, approvalErr := r.Approvals.Approve(ctx, proposal, result)
				if approvalErr != nil {
					if err := r.recordOutcome(ctx, trace, index, "approval_check_failed", "persist approval failure"); err != nil {
						return trace, err
					}
					continue
				}
				if !approved {
					if err := r.recordOutcome(ctx, trace, index, "approval_denied", "persist approval denial"); err != nil {
						return trace, err
					}
					continue
				}
			}
		}
		spec, ok := r.Registry[proposal.Operation]
		if !ok || r.Executor == nil {
			if err := r.recordOutcome(ctx, trace, index, "executor_unavailable", "persist executor availability"); err != nil {
				return trace, err
			}
			continue
		}
		trace.Proposals[index].Outcome = "executing"
		if err := r.save(ctx, trace); err != nil {
			return trace, fmt.Errorf("persist execution intent: %w", err)
		}
		operationResult, executeErr := r.Executor.Execute(ctx, spec, proposal.Args)
		if executeErr != nil {
			if err := r.recordOutcome(ctx, trace, index, "execution_failed", "persist execution failure"); err != nil {
				return trace, err
			}
			continue
		}
		trace.Proposals[index].Result = sanitizeMap(operationResult, nil)
		if r.Verifier == nil {
			if err := r.recordOutcome(ctx, trace, index, "verification_unavailable", "persist verification requirement"); err != nil {
				return trace, err
			}
			continue
		}
		verified, verifyErr := r.Verifier.Verify(ctx, trace.Observations, proposal, operationResult)
		if verifyErr != nil {
			trace.Proposals[index].Outcome = "verification_failed"
		} else if verified {
			trace.Proposals[index].Outcome = "verified"
		} else {
			trace.Proposals[index].Outcome = "not_verified"
		}
		trace.Proposals[index].Verified = &verified
		if err := r.save(ctx, trace); err != nil {
			return trace, fmt.Errorf("persist verification result: %w", err)
		}
	}
	result := "completed"
	resolution := "no operation required execution"
	needsAttention, approvalRequired, proposalReady, verifiedAny := false, false, false, false
	for _, record := range trace.Proposals {
		switch record.Outcome {
		case "verified":
			verifiedAny = true
		case "invalid_arguments", "execution_failed", "verification_failed", "not_verified", "verification_unavailable", "executor_unavailable":
			needsAttention = true
		case "approval_unavailable", "approval_check_failed":
			approvalRequired = true
		case "proposal_only":
			proposalReady = true
		case "deny", "approval_denied":
			needsAttention = true
		}
	}
	switch {
	case needsAttention:
		result, resolution = "needs_attention", "one or more proposals were denied or could not be verified"
	case approvalRequired:
		result, resolution = "approval_required", "owner approval is required before an operation can run"
	case proposalReady:
		result, resolution = "proposal_ready", "plan is recorded for review; assist mode did not execute it"
	case verifiedAny:
		result, resolution = "verified", "one or more approved operations completed and passed verification"
	}
	if trace.Result == "proposal_limit_reached" {
		result = "proposal_limit_reached"
		resolution = "one proposal was handled; additional proposals were discarded and require fresh observations"
	}
	return r.finish(ctx, trace, now, result, resolution, nil)
}

func (r CycleRunner) availableOperations() []OperationSpec {
	operations := make([]OperationSpec, 0, len(r.Registry))
	for _, spec := range r.Registry {
		if r.Policy.Capabilities[spec.Capability] {
			operations = append(operations, spec)
		}
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].Name < operations[j].Name })
	return operations
}

func (r CycleRunner) finish(ctx context.Context, trace CycleTrace, now func() time.Time, result, resolution string, cause error) (CycleTrace, error) {
	trace.FinishedAt = now().UTC()
	trace.Result = result
	trace.Resolution = resolution
	if err := r.save(ctx, trace); err != nil {
		if cause != nil {
			return trace, errors.Join(cause, fmt.Errorf("persist final audit record: %w", err))
		}
		return trace, fmt.Errorf("persist final audit record: %w", err)
	}
	return trace, cause
}

func (r CycleRunner) save(ctx context.Context, trace CycleTrace) error {
	return r.Audit.Save(ctx, trace)
}

// recordOutcome sets the outcome for the proposal at index and persists the
// trace, wrapping any persistence failure with saveErrMsg. Every call site in
// the approval/execution/verification pipeline continues to the next
// proposal on success, so callers only need to check the returned error.
func (r CycleRunner) recordOutcome(ctx context.Context, trace CycleTrace, index int, outcome, saveErrMsg string) error {
	trace.Proposals[index].Outcome = outcome
	if err := r.save(ctx, trace); err != nil {
		return fmt.Errorf("%s: %w", saveErrMsg, err)
	}
	return nil
}

func sortedCapabilities(capabilities map[Capability]bool) []Capability {
	result := make([]Capability, 0, len(capabilities))
	for capability, enabled := range capabilities {
		if enabled {
			result = append(result, capability)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func snapshotOperations(operations []OperationSpec) []OperationSnapshot {
	snapshots := make([]OperationSnapshot, 0, len(operations))
	for _, operation := range operations {
		snapshots = append(snapshots, OperationSnapshot{
			Name: operation.Name, Description: operation.Description,
			ArgsSchema: json.RawMessage(operation.ArgsSchema),
		})
	}
	return snapshots
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
