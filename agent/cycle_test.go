package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeObserver struct {
	value map[string]any
	err   error
}

func (f fakeObserver) Observe(context.Context, string, string) (map[string]any, error) {
	return f.value, f.err
}

type fakePlanner struct {
	proposals []Proposal
	input     PlanningInput
	err       error
}

func (f *fakePlanner) Plan(_ context.Context, input PlanningInput) ([]Proposal, error) {
	f.input = input
	return f.proposals, f.err
}

type fakeExecutor struct {
	calls int
	args  map[string]any
	err   error
}

func (f *fakeExecutor) Execute(_ context.Context, _ OperationSpec, args map[string]any) (map[string]any, error) {
	f.calls++
	f.args = args
	return map[string]any{"status": "active"}, f.err
}

type approvalChainFakeExecutor struct{ *fakeExecutor }

func (*approvalChainFakeExecutor) UsesAuthoritativeApprovalChain() bool { return true }

type fakeApprovals struct {
	approved bool
	calls    int
	before   func()
}

func (f *fakeApprovals) Approve(context.Context, Proposal, PolicyResult) (bool, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	return f.approved, nil
}

type fakeVerifier struct {
	verified bool
	calls    int
	observed map[string]any
}

func (f *fakeVerifier) Verify(_ context.Context, observations map[string]any, _ Proposal, _ map[string]any) (bool, error) {
	f.calls++
	f.observed = observations
	return f.verified, nil
}

type fakeAudit struct {
	saves   int
	last    CycleTrace
	history []CycleTrace
	failAt  int
	err     error
}

func (f *fakeAudit) Save(_ context.Context, trace CycleTrace) error {
	f.saves++
	if f.saves == f.failAt {
		return f.err
	}
	snapshot, err := sanitizedTraceCopy(trace)
	if err != nil {
		return err
	}
	f.last = snapshot
	f.history = append(f.history, snapshot)
	return nil
}

func testRunner(level AutonomyLevel, capability Capability, planner *fakePlanner, executor *fakeExecutor, audit *fakeAudit) CycleRunner {
	return CycleRunner{
		Policy:   Policy{Level: level, Capabilities: map[Capability]bool{capability: true}},
		Registry: DefaultRegistry(),
		Observer: fakeObserver{value: map[string]any{"service": "inactive"}},
		Planner:  planner, Executor: executor, Verifier: &fakeVerifier{verified: true}, Audit: audit,
		Now:   func() time.Time { return time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC) },
		NewID: func() (string, error) { return "test-cycle", nil },
	}
}

func TestCycleRunnerExecutesOnlyAfterPolicyAndVerifies(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "service.restart", Args: map[string]any{"name": "web"}}}}
	executor := &fakeExecutor{}
	verifier := &fakeVerifier{verified: true}
	audit := &fakeAudit{}
	runner := testRunner(Maintain, ServiceRestart, planner, executor, audit)
	runner.Verifier = verifier

	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "health_check_failed", Target: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || verifier.calls != 1 {
		t.Fatalf("executor/verifier calls = %d/%d, want 1/1", executor.calls, verifier.calls)
	}
	if trace.Result != "verified" || trace.Proposals[0].Outcome != "verified" || trace.Proposals[0].Verified == nil || !*trace.Proposals[0].Verified {
		t.Fatalf("unexpected trace result: %#v", trace)
	}
	if got := planner.input.Operations; len(got) != 1 || got[0].Name != "service.restart" {
		t.Fatalf("planner received ungranted operations: %#v", got)
	}
	if audit.last.FinishedAt.IsZero() || audit.last.ID != "test-cycle" {
		t.Fatalf("final trace was not persisted: %#v", audit.last)
	}
}

func TestCycleRunnerPassesSanitizedObservationsToVerifier(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "service.restart", Args: map[string]any{"name": "web"}}}}
	verifier := &fakeVerifier{verified: true}
	runner := testRunner(Maintain, ServiceRestart, planner, &fakeExecutor{}, &fakeAudit{})
	runner.Observer = fakeObserver{value: map[string]any{"status": "failed", "api_token": "secret"}}
	runner.Verifier = verifier
	if _, err := runner.Run(context.Background(), CycleRequest{Trigger: "health_check_failed"}); err != nil {
		t.Fatal(err)
	}
	if verifier.observed["api_token"] != redactedValue {
		t.Fatalf("verifier received unsanitized observations: %#v", verifier.observed)
	}
}

func TestCycleRunnerAssistNeverExecutes(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.health", Args: map[string]any{"app": "photos"}}}}
	executor := &fakeExecutor{}
	runner := testRunner(Assist, HealthRead, planner, executor, &fakeAudit{})
	runner.Observer = fakeObserver{value: map[string]any{"app": "photos", "api_token": "must-not-reach-planner"}}
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 || trace.Proposals[0].Outcome != string(DecisionProposalOnly) {
		t.Fatalf("assist executed or lost proposal: calls=%d trace=%#v", executor.calls, trace)
	}
	if planner.input.Observations["api_token"] != redactedValue {
		t.Fatalf("planner received unsanitized observations: %#v", planner.input.Observations)
	}
}

func TestCycleRunnerInjectsRetrievedKnowledgeAndAuditsCitation(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.health", Args: map[string]any{"app": "photos"}}}}
	audit := &fakeAudit{}
	retriever, err := NewLocalRetriever([]KnowledgeDocument{{
		ID: "incident-17", Source: "verified-trace", Text: "A failed photo service recovered after checking its database dependency.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := testRunner(Assist, HealthRead, planner, &fakeExecutor{}, audit)
	runner.Retriever = retriever
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "photo service unhealthy", Target: "photos"})
	if err != nil {
		t.Fatal(err)
	}
	if len(planner.input.Knowledge) != 1 || planner.input.Knowledge[0].ID != "incident-17" {
		t.Fatalf("planner did not receive local context: %#v", planner.input.Knowledge)
	}
	if len(trace.Knowledge) != 1 || trace.Knowledge[0].Hash != planner.input.Knowledge[0].Hash {
		t.Fatalf("trace did not record retrieved source version: %#v", trace.Knowledge)
	}
}

func TestCycleRunnerRequiresApprovalForDestructiveOperation(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.restore", Args: map[string]any{"app": "photos", "snapshot": "snapshot-1"}}}}
	executor := &fakeExecutor{}
	approvals := &fakeApprovals{approved: false}
	runner := testRunner(Autonomous, AppRestore, planner, executor, &fakeAudit{})
	runner.Approvals = approvals
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "owner_request"})
	if err != nil {
		t.Fatal(err)
	}
	if approvals.calls != 1 || executor.calls != 0 || trace.Proposals[0].Outcome != "approval_denied" {
		t.Fatalf("destructive op crossed approval boundary: approvals=%d executor=%d trace=%#v", approvals.calls, executor.calls, trace)
	}
}

func TestCycleRunnerDoesNotDelegateApprovalToGenericExecutor(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.restore", Args: map[string]any{"app": "photos", "snapshot": "snapshot-1"}}}}
	executor := &fakeExecutor{}
	runner := testRunner(Autonomous, AppRestore, planner, executor, &fakeAudit{})
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "owner_request"})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 || trace.Proposals[0].Outcome != "approval_unavailable" {
		t.Fatalf("generic executor crossed the approval boundary: calls=%d trace=%#v", executor.calls, trace)
	}
}

func TestCycleRunnerDelegatesRequestBoundApprovalToNostrHost(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.restore", Args: map[string]any{"app": "photos", "snapshot": "snapshot-1"}}}}
	audit := &fakeAudit{}
	operation := &approvalChainFakeExecutor{fakeExecutor: &fakeExecutor{}}
	runner := testRunner(Autonomous, AppRestore, planner, operation.fakeExecutor, audit)
	runner.Executor = operation
	runner.Verifier = &fakeVerifier{verified: true}
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "owner_request"})
	if err != nil {
		t.Fatal(err)
	}
	if operation.calls != 1 || trace.Proposals[0].Outcome != "verified" {
		t.Fatalf("delegated approval did not pass through the control plane: calls=%d trace=%#v", operation.calls, trace)
	}
	foundDelegatedCheckpoint := false
	for _, checkpoint := range audit.history {
		if len(checkpoint.Proposals) == 1 && checkpoint.Proposals[0].Outcome == "approval_delegated" {
			foundDelegatedCheckpoint = true
		}
	}
	if !foundDelegatedCheckpoint {
		t.Fatal("approval delegation was not audited before request dispatch")
	}
}

func TestCycleRunnerPersistsApprovalRequestBeforeExecution(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.restore", Args: map[string]any{"app": "photos", "snapshot": "snapshot-1"}}}}
	executor := &fakeExecutor{}
	verifier := &fakeVerifier{verified: true}
	audit := &fakeAudit{}
	approvals := &fakeApprovals{approved: true}
	approvals.before = func() {
		if audit.last.ID != "test-cycle" || len(audit.last.Proposals) != 1 || audit.last.Proposals[0].Policy.Decision != DecisionApproval {
			t.Errorf("approval was requested before its decision was audited: %#v", audit.last)
		}
	}
	runner := testRunner(Autonomous, AppRestore, planner, executor, audit)
	runner.Approvals = approvals
	runner.Verifier = verifier
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "owner_request"})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || trace.Proposals[0].Outcome != "verified" {
		t.Fatalf("approved operation did not complete: calls=%d trace=%#v", executor.calls, trace)
	}
}

func TestCycleRunnerAuditFailureBeforeExecutionStopsOperation(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "service.restart"}}}
	executor := &fakeExecutor{}
	audit := &fakeAudit{failAt: 4, err: errors.New("disk unavailable")}
	_, err := testRunner(Maintain, ServiceRestart, planner, executor, audit).Run(context.Background(), CycleRequest{Trigger: "health_check_failed"})
	if err == nil {
		t.Fatal("expected audit persistence failure")
	}
	if executor.calls != 0 {
		t.Fatalf("operation ran after execution-intent audit failure: %d calls", executor.calls)
	}
}

func TestCycleRunnerObserveDoesNotInvokePlanner(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "app.health"}}}
	audit := &fakeAudit{}
	trace, err := testRunner(Observe, HealthRead, planner, &fakeExecutor{}, audit).Run(context.Background(), CycleRequest{Trigger: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if planner.input.Trigger != "" || len(trace.Proposals) != 0 || trace.Result != "observed" {
		t.Fatalf("observe mode planned work: input=%#v trace=%#v", planner.input, trace)
	}
}

func TestCycleRunnerTruncatesExcessPlannerOutput(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{
		{Operation: "app.health", Args: map[string]any{"app": "photos"}},
		{Operation: "app.health", Args: map[string]any{"app": "photos"}},
	}}
	runner := testRunner(Assist, HealthRead, planner, &fakeExecutor{}, &fakeAudit{})
	runner.MaxProposals = 8 // configuration cannot raise the per-cycle action bound
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Proposals) != 1 || trace.Result != "proposal_limit_reached" {
		t.Fatalf("proposal limit not enforced: %#v", trace)
	}
}

func TestCycleRunnerRejectsInvalidArgsBeforeApprovalOrExecution(t *testing.T) {
	planner := &fakePlanner{proposals: []Proposal{{Operation: "service.restart", Args: map[string]any{"name": "web", "command": "rm -rf /"}}}}
	executor := &fakeExecutor{}
	approvals := &fakeApprovals{approved: true}
	runner := testRunner(Autonomous, ServiceRestart, planner, executor, &fakeAudit{})
	runner.Approvals = approvals
	trace, err := runner.Run(context.Background(), CycleRequest{Trigger: "health_check_failed"})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 || approvals.calls != 0 || trace.Proposals[0].Outcome != "invalid_arguments" || trace.Proposals[0].Proposal.Args != nil {
		t.Fatalf("invalid arguments crossed the operation boundary: approvals=%d executor=%d trace=%#v", approvals.calls, executor.calls, trace)
	}
}

func TestDefaultRegistryPublishesAndEnforcesSchemas(t *testing.T) {
	registry := DefaultRegistry()
	for name, spec := range registry {
		if !json.Valid([]byte(spec.ArgsSchema)) {
			t.Errorf("%s has invalid JSON schema %q", name, spec.ArgsSchema)
		}
		if err := spec.ValidateArgs(map[string]any{}); err != nil && name == "system.health" {
			t.Errorf("empty-argument read operation rejected: %v", err)
		}
	}
	if err := registry["service.restart"].ValidateArgs(map[string]any{"name": "nginx"}); err != nil {
		t.Fatalf("valid service restart args rejected: %v", err)
	}
	if err := registry["service.restart"].ValidateArgs(map[string]any{"name": "nginx", "shell": "true"}); err == nil {
		t.Fatal("unknown service restart argument accepted")
	}
}

func TestOperationArgumentsHaveHardSizeLimit(t *testing.T) {
	args := map[string]any{"app": strings.Repeat("a", maxOperationArgsBytes)}
	if err := DefaultRegistry()["app.health"].ValidateArgs(args); err == nil {
		t.Fatal("oversized operation arguments accepted")
	}
}

func TestAvailableOperationsAreDeterministic(t *testing.T) {
	runner := testRunner(Maintain, HealthRead, &fakePlanner{}, &fakeExecutor{}, &fakeAudit{})
	got := runner.availableOperations()
	names := make([]string, len(got))
	for i := range got {
		names[i] = got[i].Name
	}
	want := []string{"app.health", "system.health"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("operations = %#v, want %#v", names, want)
	}
}
