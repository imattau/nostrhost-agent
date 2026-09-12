package agent

import (
	"encoding/json"
	"time"
)

// CycleTrace is the structured audit record for one Observe → Diagnose →
// Plan → Execute → Verify cycle. Callers should persist a copy before and
// after execution so a process crash does not erase proposed work.
type CycleTrace struct {
	ID                  string              `json:"id"`
	StartedAt           time.Time           `json:"started_at"`
	FinishedAt          time.Time           `json:"finished_at,omitempty"`
	Trigger             string              `json:"trigger"`
	Target              string              `json:"target,omitempty"`
	Observations        map[string]any      `json:"observations,omitempty"`
	Capabilities        []Capability        `json:"capabilities,omitempty"`
	AvailableOperations []OperationSnapshot `json:"available_operations,omitempty"`
	PlanningCompleted   bool                `json:"planning_completed,omitempty"`
	Knowledge           []KnowledgeCitation `json:"knowledge,omitempty"`
	Proposals           []ProposalRecord    `json:"proposals,omitempty"`
	Result              string              `json:"result"`
	Resolution          string              `json:"resolution,omitempty"`
}

// OperationSnapshot records the exact model-visible tool schema at the
// planner decision point. It is evidence for later review, not an executable
// operation definition.
type OperationSnapshot struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	ArgsSchema  json.RawMessage `json:"args_schema"`
}

type ProposalRecord struct {
	Proposal Proposal       `json:"proposal"`
	Policy   PolicyResult   `json:"policy"`
	Outcome  string         `json:"outcome,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Verified *bool          `json:"verified,omitempty"`
}
