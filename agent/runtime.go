package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type RuntimeConfig struct {
	Relay               RelayTransportConfig
	Inference           LLMPlannerConfig
	Embeddings          EmbeddingConfig
	Registry            map[string]OperationSpec
	Policy              Policy
	ObservationQueries  []ObservationQuery
	VerificationRules   []VerificationRule
	KnowledgeCorpusPath string
	AuditPath           string
	Interval            time.Duration
	RunImmediately      bool
	ListenForEvents     bool
	EventLookback       time.Duration
	Triggers            <-chan CycleRequest
	Approvals           ApprovalGate
	Verifier            Verifier
	OnCycle             func(CycleTrace, error)
	Contribution        ContributionFileConfig
}

// ResidentRuntime owns the resources needed by one configured resident agent.
// Its lifecycle is single-run; Run closes owned resources on exit and Close
// supports cleanup when the runtime is constructed but never started.
type ResidentRuntime struct {
	mu          sync.Mutex
	service     ResidentService
	eventSource *NostrEventTriggerSource
	executor    *NostrOperationExecutor
	audit       *JSONLAuditSink
	started     bool
	closing     bool
	closed      bool
	cancel      context.CancelFunc
	done        chan struct{}
	closeErr    error
}

// ValidateRuntimeConfig checks policy, registry, trigger, and audit settings
// without opening files or constructing network resources. It is also used by
// the package's explicit pre-enable config check.
func ValidateRuntimeConfig(cfg RuntimeConfig) error {
	registry := cfg.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}
	if err := ValidateRegistry(registry); err != nil {
		return fmt.Errorf("invalid operation registry: %w", err)
	}
	if _, ok := autonomyRank[cfg.Policy.Level]; !ok {
		return fmt.Errorf("unknown autonomy level %q", cfg.Policy.Level)
	}
	if cfg.Interval < 0 {
		return errors.New("maintenance interval cannot be negative")
	}
	if cfg.EventLookback < 0 || cfg.EventLookback > 24*time.Hour {
		return errors.New("event trigger lookback must be between zero and 24 hours")
	}
	if cfg.ListenForEvents && cfg.Triggers != nil {
		return errors.New("configure either Nostr events or an external trigger channel, not both")
	}
	if cfg.AuditPath == "" {
		return errors.New("local audit journal path is required")
	}
	for _, query := range cfg.ObservationQueries {
		spec, exists := registry[query.Operation]
		if exists {
			for _, scope := range spec.Scopes {
				if !cfg.Policy.Scopes[scope] {
					return fmt.Errorf("observation query %q requires disabled local scope %q", query.Operation, scope)
				}
			}
		}
	}
	for _, rule := range cfg.VerificationRules {
		spec, exists := registry[rule.CheckOperation]
		if exists {
			for _, scope := range spec.Scopes {
				if !cfg.Policy.Scopes[scope] {
					return fmt.Errorf("verification check %q requires disabled local scope %q", rule.CheckOperation, scope)
				}
			}
		}
	}
	return nil
}

func NewResidentRuntime(cfg RuntimeConfig) (*ResidentRuntime, error) {
	if err := ValidateRuntimeConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid resident runtime configuration: %w", err)
	}
	registry := cfg.Registry
	if registry == nil {
		registry = DefaultRegistry()
	}

	executor, err := NewControlPlaneExecutor(cfg.Relay, registry)
	if err != nil {
		return nil, fmt.Errorf("create Nostr operation executor: %w", err)
	}
	cleanupExecutor := true
	defer func() {
		if cleanupExecutor {
			executor.Close()
		}
	}()

	observer, err := NewNostrOperationObserver(executor, registry, cfg.ObservationQueries)
	if err != nil {
		return nil, fmt.Errorf("create Nostr operation observer: %w", err)
	}
	var eventSource *NostrEventTriggerSource
	if cfg.ListenForEvents {
		relayTransport, ok := executor.Transport.(*RelayOperationTransport)
		if !ok {
			return nil, errors.New("resident runtime requires its concrete relay transport for event triggers")
		}
		eventSource, err = NewNostrEventTriggerSource(relayTransport, cfg.EventLookback)
		if err != nil {
			return nil, fmt.Errorf("create Nostr event trigger source: %w", err)
		}
	}
	var planner Planner
	if cfg.Policy.Level != Observe {
		planner, err = NewOpenAICompatiblePlanner(cfg.Inference)
		if err != nil {
			return nil, fmt.Errorf("create local planner: %w", err)
		}
	}
	var embedder Embedder
	if cfg.Embeddings.BaseURL != "" || cfg.Embeddings.Model != "" || cfg.Embeddings.APIKey != "" {
		localEmbedder, embedErr := NewOpenAICompatibleEmbedder(cfg.Embeddings)
		if embedErr != nil {
			return nil, fmt.Errorf("create local semantic embedder: %w", embedErr)
		}
		embedder = localEmbedder
	}
	verifier := cfg.Verifier
	if verifier == nil && autonomyRank[cfg.Policy.Level] >= autonomyRank[Maintain] {
		verifier, err = NewNostrOperationVerifier(executor, registry, cfg.VerificationRules)
		if err != nil {
			return nil, fmt.Errorf("create Nostr operation verifier: %w", err)
		}
	}
	audit, err := OpenJSONLAuditSink(cfg.AuditPath)
	if err != nil {
		return nil, fmt.Errorf("open local audit journal: %w", err)
	}
	cleanupAudit := true
	defer func() {
		if cleanupAudit {
			_ = audit.Close()
		}
	}()

	var documents []KnowledgeDocument
	if cfg.KnowledgeCorpusPath != "" {
		loaded, loadErr := LoadKnowledgeDocuments(cfg.KnowledgeCorpusPath)
		if loadErr != nil {
			return nil, fmt.Errorf("load local knowledge corpus: %w", loadErr)
		}
		documents = loaded
	}
	retriever, err := NewVerifiedHistoryRetriever(documents, audit, embedder)
	if err != nil {
		return nil, fmt.Errorf("create verified history retriever: %w", err)
	}
	scopes := make(map[Scope]bool, len(cfg.Policy.Scopes))
	for scope, enabled := range cfg.Policy.Scopes {
		scopes[scope] = enabled
	}
	runner := CycleRunner{
		Policy:   Policy{Level: cfg.Policy.Level, Scopes: scopes},
		Registry: registry, Observer: observer, Planner: planner, Retriever: retriever,
		Executor: executor, Approvals: cfg.Approvals, Verifier: verifier,
		Audit: audit,
	}
	onCycle := cfg.OnCycle
	if cfg.Contribution.Enabled {
		submitter, err := NewContributionAutoSubmitter(cfg.Contribution)
		if err != nil {
			return nil, fmt.Errorf("initialize automatic contribution submitter: %w", err)
		}
		previous := onCycle
		onCycle = func(trace CycleTrace, cycleErr error) {
			if previous != nil {
				previous(trace, cycleErr)
			}
			submitter.OnCycle(trace, cycleErr)
		}
	}
	service := ResidentService{
		Runner: runner, Interval: cfg.Interval, RunImmediately: cfg.RunImmediately,
		Triggers: cfg.Triggers, OnCycle: onCycle,
	}
	if err := service.validate(); err != nil {
		return nil, fmt.Errorf("invalid resident runtime configuration: %w", err)
	}
	cleanupExecutor = false
	cleanupAudit = false
	return &ResidentRuntime{service: service, eventSource: eventSource, executor: executor, audit: audit}, nil
}

func (r *ResidentRuntime) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("resident runtime is not initialized")
	}
	if ctx == nil {
		return errors.New("resident runtime requires a context")
	}
	r.mu.Lock()
	if r.started || r.closing || r.closed {
		r.mu.Unlock()
		return errors.New("resident runtime can only be run once")
	}
	r.started = true
	child, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	done := r.done
	r.mu.Unlock()

	service := r.service
	if r.eventSource != nil {
		service.Triggers = r.eventSource.Start(child)
	}
	runErr := service.Run(child)
	cancel()
	closeErr := r.closeOwnedResources()
	r.mu.Lock()
	r.closed = true
	r.closeErr = closeErr
	close(done)
	r.mu.Unlock()
	return errors.Join(runErr, closeErr)
}

func (r *ResidentRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.started || r.closing {
		if r.cancel != nil {
			r.cancel()
		}
		done := r.done
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closing = true
	r.done = make(chan struct{})
	done := r.done
	r.mu.Unlock()

	err := r.closeOwnedResources()
	r.mu.Lock()
	r.closed = true
	r.closing = false
	r.closeErr = err
	close(done)
	r.mu.Unlock()
	return err
}

func (r *ResidentRuntime) closeOwnedResources() error {
	var closeErrors []error
	if r.executor != nil {
		r.executor.Close()
	}
	if r.audit != nil {
		if err := r.audit.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close local audit journal: %w", err))
		}
	}
	return errors.Join(closeErrors...)
}
