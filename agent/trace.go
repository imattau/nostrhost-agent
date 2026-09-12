package agent

import "time"

// CycleTrace is the structured audit record for one Observe → Diagnose →
// Plan → Execute → Verify cycle. Callers should persist a copy before and
// after execution so a process crash does not erase proposed work.
type CycleTrace struct {
	ID           string              `json:"id"`
	StartedAt    time.Time           `json:"started_at"`
	FinishedAt   time.Time           `json:"finished_at,omitempty"`
	Trigger      string              `json:"trigger"`
	Target       string              `json:"target,omitempty"`
	Observations map[string]any      `json:"observations,omitempty"`
	Capabilities []Capability        `json:"capabilities,omitempty"`
	Knowledge    []KnowledgeCitation `json:"knowledge,omitempty"`
	Proposals    []ProposalRecord    `json:"proposals,omitempty"`
	Result       string              `json:"result"`
	Resolution   string              `json:"resolution,omitempty"`
}

type ProposalRecord struct {
	Proposal Proposal       `json:"proposal"`
	Policy   PolicyResult   `json:"policy"`
	Outcome  string         `json:"outcome,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Verified *bool          `json:"verified,omitempty"`
}
