package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const scheduledMaintenanceTrigger = "scheduled_maintenance"

// ResidentService owns the process lifecycle around CycleRunner. It handles
// one cycle at a time; periodic ticks coalesce while a cycle is running.
type ResidentService struct {
	Runner         CycleRunner
	Interval       time.Duration
	RunImmediately bool
	Triggers       <-chan CycleRequest
	OnCycle        func(CycleTrace, error)
}

func (s ResidentService) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("resident service requires a context")
	}
	if err := s.validate(); err != nil {
		return err
	}
	var ticker *time.Ticker
	var ticks <-chan time.Time
	if s.Interval > 0 {
		ticker = time.NewTicker(s.Interval)
		ticks = ticker.C
		defer ticker.Stop()
	}
	if s.RunImmediately {
		if err := s.runCycle(ctx, CycleRequest{Trigger: "service_started"}); err != nil {
			return err
		}
	}
	triggers := s.Triggers
	for {
		select {
		case <-ctx.Done():
			return nil
		case request, open := <-triggers:
			if !open {
				triggers = nil
				continue
			}
			if err := s.runCycle(ctx, request); err != nil {
				return err
			}
		case <-ticks:
			if err := s.runCycle(ctx, CycleRequest{Trigger: scheduledMaintenanceTrigger}); err != nil {
				return err
			}
		}
	}
}

func (s ResidentService) validate() error {
	if s.Interval < 0 {
		return errors.New("maintenance interval cannot be negative")
	}
	if s.Runner.Observer == nil || s.Runner.Audit == nil {
		return errors.New("resident service requires an observer and audit sink")
	}
	if err := ValidateRegistry(s.Runner.Registry); err != nil {
		return fmt.Errorf("invalid operation registry: %w", err)
	}
	if _, ok := autonomyRank[s.Runner.Policy.Level]; !ok {
		return fmt.Errorf("unknown autonomy level %q", s.Runner.Policy.Level)
	}
	if s.Runner.Policy.Level != Observe && s.Runner.Planner == nil {
		return errors.New("resident service requires a planner outside observe mode")
	}
	if autonomyRank[s.Runner.Policy.Level] >= autonomyRank[Maintain] && s.Runner.Executor == nil {
		return errors.New("resident service requires an executor in maintenance modes")
	}
	if autonomyRank[s.Runner.Policy.Level] >= autonomyRank[Maintain] && s.Runner.Verifier == nil {
		return errors.New("resident service requires a verifier in maintenance modes")
	}
	return nil
}

func (s ResidentService) runCycle(ctx context.Context, request CycleRequest) error {
	trace, err := s.Runner.Run(ctx, request)
	if s.OnCycle != nil {
		s.OnCycle(trace, err)
	}
	if err != nil {
		return fmt.Errorf("resident service stopped after cycle %q failed: %w", trace.ID, err)
	}
	return nil
}
