package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResidentServiceRunsStartupCycleAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := ResidentService{
		Runner:         testRunner(Observe, HealthRead, nil, nil, &fakeAudit{}),
		RunImmediately: true,
		OnCycle: func(trace CycleTrace, err error) {
			if err != nil || trace.Result != "observed" || trace.Trigger != "service_started" {
				t.Errorf("unexpected startup cycle: trace=%#v err=%v", trace, err)
			}
			cancel()
		},
	}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestResidentServiceHandlesReactiveAndPeriodicTriggers(t *testing.T) {
	t.Run("reactive", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		triggers := make(chan CycleRequest, 1)
		triggers <- CycleRequest{Trigger: "health_check_failed", Target: "web"}
		service := ResidentService{
			Runner:   testRunner(Observe, HealthRead, nil, nil, &fakeAudit{}),
			Triggers: triggers,
			OnCycle: func(trace CycleTrace, err error) {
				if err != nil || trace.Trigger != "health_check_failed" || trace.Target != "web" {
					t.Errorf("unexpected reactive cycle: trace=%#v err=%v", trace, err)
				}
				cancel()
			},
		}
		if err := service.Run(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("periodic", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := ResidentService{
			Runner:   testRunner(Observe, HealthRead, nil, nil, &fakeAudit{}),
			Interval: 5 * time.Millisecond,
			OnCycle: func(trace CycleTrace, err error) {
				if err != nil || trace.Trigger != scheduledMaintenanceTrigger {
					t.Errorf("unexpected scheduled cycle: trace=%#v err=%v", trace, err)
				}
				cancel()
			},
		}
		if err := service.Run(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestResidentServiceStopsOnCycleError(t *testing.T) {
	triggers := make(chan CycleRequest, 1)
	triggers <- CycleRequest{Trigger: "scheduled"}
	service := ResidentService{
		Runner:   testRunner(Observe, HealthRead, nil, nil, &fakeAudit{failAt: 1, err: errors.New("audit unavailable")}),
		Triggers: triggers,
	}
	err := service.Run(context.Background())
	if err == nil {
		t.Fatal("resident service continued after cycle audit failure")
	}
}

func TestResidentServicePreflightsMaintenanceRequirements(t *testing.T) {
	runner := testRunner(Maintain, ServiceRestart, &fakePlanner{}, nil, &fakeAudit{})
	runner.Executor = nil
	runner.Verifier = nil
	service := ResidentService{Runner: runner}
	if err := service.validate(); err == nil {
		t.Fatal("maintenance service without executor or verifier passed preflight")
	}
	service.Runner.Executor = &fakeExecutor{}
	if err := service.validate(); err == nil {
		t.Fatal("maintenance service without verifier passed preflight")
	}
}
