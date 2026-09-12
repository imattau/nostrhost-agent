package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
)

const (
	contributionSchemaVersion = "nostrhost-agent-contribution/v1"
	maxContributionJournal    = 256 << 20
)

// ContributionCandidate is a local review artifact, never a training label.
// Human reviewers must inspect, edit, or discard it before any sharing.
type ContributionCandidate struct {
	SchemaVersion         string                `json:"schema_version"`
	CandidateID           string                `json:"candidate_id"`
	SourceRef             string                `json:"source_ref"`
	ReviewStatus          string                `json:"review_status"`
	PrivacyReviewRequired bool                  `json:"privacy_review_required"`
	RedactionsApplied     int                   `json:"redactions_applied"`
	PlannerInput          ContributionInput     `json:"planner_input"`
	ObservedDecision      ContributionDecision  `json:"observed_decision"`
	ExpectedDecision      *ContributionDecision `json:"expected_decision"`
	Outcome               ContributionOutcome   `json:"outcome"`
	ReviewWarning         string                `json:"review_warning"`
}

type ContributionInput struct {
	Trigger             string              `json:"trigger"`
	Target              string              `json:"target,omitempty"`
	Observations        map[string]any      `json:"observations,omitempty"`
	AvailableOperations []OperationSnapshot `json:"available_operations"`
}

type ContributionDecision struct {
	NoCall    bool           `json:"no_call"`
	Operation string         `json:"operation,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Policy    string         `json:"policy,omitempty"`
}

type ContributionOutcome struct {
	CycleResult string                 `json:"cycle_result"`
	Proposals   []ContributionProposal `json:"proposals,omitempty"`
}

type ContributionProposal struct {
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Policy    string         `json:"policy"`
	Outcome   string         `json:"outcome,omitempty"`
	Verified  *bool          `json:"verified,omitempty"`
}

// ReadContributionCycle reads a journal without opening it for writing or
// repairing an interrupted tail. It returns the latest snapshot of cycleID.
func ReadContributionCycle(path, cycleID string) (CycleTrace, error) {
	if strings.TrimSpace(cycleID) == "" {
		return CycleTrace{}, errors.New("cycle id is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return CycleTrace{}, fmt.Errorf("inspect audit journal: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return CycleTrace{}, errors.New("audit journal must be a regular file, not a symlink")
	}
	if before.Mode().Perm()&0o077 != 0 {
		return CycleTrace{}, errors.New("audit journal has group or other permissions; secure it to mode 0600 first")
	}
	if before.Size() > maxContributionJournal {
		return CycleTrace{}, errors.New("audit journal exceeds the 256 MiB export limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return CycleTrace{}, fmt.Errorf("open audit journal read-only: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return CycleTrace{}, errors.New("audit journal changed while it was being opened")
	}
	if after.Size() > maxContributionJournal {
		return CycleTrace{}, errors.New("audit journal exceeds the 256 MiB export limit")
	}
	if after.Size() == 0 {
		return CycleTrace{}, errors.New("audit journal is empty")
	}
	var lastByte [1]byte
	if _, err := file.ReadAt(lastByte[:], after.Size()-1); err != nil || lastByte[0] != '\n' {
		return CycleTrace{}, errors.New("audit journal has an incomplete final record; export will not repair it")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return CycleTrace{}, fmt.Errorf("read audit journal: %w", err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxAuditRecordBytes+1)
	var selected CycleTrace
	found := false
	for scanner.Scan() {
		var trace CycleTrace
		if err := json.Unmarshal(scanner.Bytes(), &trace); err != nil || trace.ID == "" {
			return CycleTrace{}, errors.New("audit journal contains an invalid record")
		}
		if trace.ID == cycleID {
			selected, found = trace, true
		}
	}
	if err := scanner.Err(); err != nil {
		return CycleTrace{}, fmt.Errorf("read audit journal: %w", err)
	}
	if !found {
		return CycleTrace{}, errors.New("selected cycle id was not found in the audit journal")
	}
	if selected.FinishedAt.IsZero() || !selected.PlanningCompleted {
		return CycleTrace{}, errors.New("selected cycle is not a finalized planner decision")
	}
	if len(selected.AvailableOperations) == 0 {
		return CycleTrace{}, errors.New("selected cycle is missing its model-visible operation schemas")
	}
	return selected, nil
}

// BuildContributionCandidate creates a sanitized, model-neutral review
// candidate. The observed planner output is explicitly not accepted as truth.
func BuildContributionCandidate(trace CycleTrace) (ContributionCandidate, error) {
	if trace.ID == "" || trace.FinishedAt.IsZero() || !trace.PlanningCompleted {
		return ContributionCandidate{}, errors.New("a finalized planner decision is required")
	}
	if len(trace.AvailableOperations) == 0 {
		return ContributionCandidate{}, errors.New("model-visible operation schemas are required")
	}
	if len(trace.Proposals) > 1 {
		return ContributionCandidate{}, errors.New("cycle contains more proposals than the agent's supported limit")
	}
	redactor := contributionRedactor{}
	input := ContributionInput{
		Trigger:             redactor.text(trace.Trigger),
		Target:              "",
		Observations:        redactor.value(trace.Observations, "observations").(map[string]any),
		AvailableOperations: make([]OperationSnapshot, 0, len(trace.AvailableOperations)),
	}
	if len(trace.Observations) == 0 {
		input.Observations = nil
	}
	for _, operation := range trace.AvailableOperations {
		if operation.Name == "" || len(operation.ArgsSchema) == 0 || !json.Valid(operation.ArgsSchema) {
			return ContributionCandidate{}, errors.New("cycle contains an invalid operation schema")
		}
		input.AvailableOperations = append(input.AvailableOperations, OperationSnapshot{
			Name: operation.Name, Description: redactor.text(operation.Description),
			ArgsSchema: append(json.RawMessage(nil), operation.ArgsSchema...),
		})
	}
	knownOperations := make(map[string]bool, len(input.AvailableOperations))
	for _, operation := range input.AvailableOperations {
		knownOperations[operation.Name] = true
	}
	decision := ContributionDecision{NoCall: len(trace.Proposals) == 0}
	outcome := ContributionOutcome{CycleResult: safeCycleResult(trace.Result)}
	for _, proposal := range trace.Proposals {
		operation := proposal.Proposal.Operation
		if operation == "" || !knownOperations[operation] {
			operation = "unknown"
		}
		args := redactor.value(proposal.Proposal.Args, "arguments")
		argumentMap, _ := args.(map[string]any)
		policy := safePolicyDecision(proposal.Policy.Decision)
		record := ContributionProposal{
			Operation: operation, Arguments: argumentMap, Policy: policy,
			Outcome: safeProposalOutcome(proposal.Outcome), Verified: proposal.Verified,
		}
		outcome.Proposals = append(outcome.Proposals, record)
		decision = ContributionDecision{NoCall: false, Operation: operation, Arguments: argumentMap, Policy: policy}
	}
	if decision.Arguments == nil && !decision.NoCall {
		decision.Arguments = map[string]any{}
	}
	refHash := sha256.Sum256([]byte("nostrhost-agent-contribution-v1:" + trace.ID))
	ref := hex.EncodeToString(refHash[:])
	candidate := ContributionCandidate{
		SchemaVersion: contributionSchemaVersion,
		CandidateID:   "candidate-" + ref[:20], SourceRef: "sha256:" + ref,
		ReviewStatus: "submitted", PrivacyReviewRequired: true,
		PlannerInput: input, ObservedDecision: decision, ExpectedDecision: nil,
		Outcome:       outcome,
		ReviewWarning: "Local review candidate only. Inspect for identifying or sensitive data, correct the expected decision using independent evidence, and discard if uncertain. No data has been uploaded.",
	}
	candidate.RedactionsApplied = redactor.count
	return candidate, nil
}

func WriteContributionCandidate(path string, candidate ContributionCandidate) error {
	data, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return fmt.Errorf("encode contribution candidate: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create contribution candidate without overwriting: %w", err)
	}
	created := true
	defer func() {
		file.Close()
		if created {
			os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure contribution candidate: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write contribution candidate: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync contribution candidate: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close contribution candidate: %w", err)
	}
	created = false
	return nil
}

var contributionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:nsec|npub|note|nevent|nprofile|naddr)1[023456789acdefghjklmnpqrstuvwxyz]{20,}\b`),
	regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`),
	regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`),
	regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`),
	regexp.MustCompile(`(?i)(?:^|[\s=:])(?:[0-9a-f]{2}:){5}[0-9a-f]{2}\b`),
	regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+(?:com|net|org|io|dev|app|local|home|lan|test|example|internal|invalid|corp|onion|nostr|au|uk|edu|gov)\b`),
	regexp.MustCompile(`(?:^|\s)/(?:home|root|tmp|var|etc|opt|srv|mnt|media)/[^\s,;"']+`),
	regexp.MustCompile(`(?i)\b(?:bearer|token|api[_-]?key|password|passwd|secret|authorization)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{40,}\b`),
}

var possibleIPv6Pattern = regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,}[0-9a-f:]{0,}`)

type contributionRedactor struct{ count int }

func (r *contributionRedactor) text(value string) string {
	for _, pattern := range contributionPatterns {
		matches := pattern.FindAllString(value, -1)
		if len(matches) == 0 {
			continue
		}
		r.count += len(matches)
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	value = possibleIPv6Pattern.ReplaceAllStringFunc(value, func(match string) string {
		if net.ParseIP(strings.Trim(match, ":")) == nil {
			return match
		}
		r.count++
		return "[REDACTED]"
	})
	return value
}

func (r *contributionRedactor) value(value any, key string) any {
	if isContributionSensitiveKey(key) || isContributionIdentityKey(key) {
		if value == nil {
			return nil
		}
		r.count++
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			cleanKey := r.text(k)
			out[cleanKey] = r.value(v, k)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = r.value(item, "")
		}
		return out
	case string:
		return r.text(typed)
	default:
		return value
	}
}

func isContributionSensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	if isSensitiveKey(normalized) {
		return true
	}
	for _, marker := range []string{"password", "passwd", "token", "secret", "private_key", "api_key", "authorization", "credential", "cookie", "nsec"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func isContributionIdentityKey(key string) bool {
	normalized := normalizeKey(key)
	switch normalized {
	case "name", "app", "host", "hostname", "domain", "user", "username", "email", "ip", "address", "path", "url", "npub", "pubkey", "service_instance", "instance_id":
		return true
	}
	for _, marker := range []string{"_name", "_app", "_host", "_hostname", "_domain", "_user", "_username", "_email", "_ip", "_address", "_path", "_url", "instance_id"} {
		if strings.HasSuffix(normalized, marker) {
			return true
		}
	}
	return false
}

func safeCycleResult(value string) string {
	switch value {
	case "completed", "needs_attention", "approval_required", "proposal_ready", "verified", "proposal_limit_reached", "interrupted":
		return value
	default:
		return "unknown"
	}
}

func safePolicyDecision(value Decision) string {
	switch value {
	case DecisionObserveOnly, DecisionProposalOnly, DecisionApproval, DecisionAllow, DecisionDeny:
		return string(value)
	default:
		return "unknown"
	}
}

func safeProposalOutcome(value string) string {
	switch value {
	case "deny", "allow", "observe_only", "proposal_only", "approval_required", "approval_unavailable", "approval_delegated", "approval_check_failed", "approval_denied", "executor_unavailable", "executing", "execution_failed", "verification_unavailable", "verification_failed", "not_verified", "verified", "invalid_arguments":
		return value
	default:
		return "unknown"
	}
}
