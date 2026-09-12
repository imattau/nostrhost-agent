package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordingOperationExecutor struct {
	operations []string
	args       []map[string]any
	results    map[string]map[string]any
	failures   map[string]error
}

func (e *recordingOperationExecutor) Execute(_ context.Context, spec OperationSpec, args map[string]any) (map[string]any, error) {
	e.operations = append(e.operations, spec.Name)
	cloned, _ := cloneJSONMap(args)
	e.args = append(e.args, cloned)
	if err := e.failures[spec.Name]; err != nil {
		return nil, err
	}
	return e.results[spec.Name], nil
}

func TestNostrOperationObserverBuildsReadModelFromSelectedOperations(t *testing.T) {
	executor := &recordingOperationExecutor{results: map[string]map[string]any{
		"app.health":  {"status": "healthy"},
		"disk.status": {"free_bytes": 1024},
	}}
	observer, err := NewNostrOperationObserver(executor, nil, []ObservationQuery{
		{Operation: "app.health", TargetArg: "app"},
		{Operation: "disk.status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	observations, err := observer.Observe(context.Background(), "scheduled", "photos")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executor.operations, []string{"app.health", "disk.status"}) {
		t.Fatalf("unexpected read operation sequence: %#v", executor.operations)
	}
	if executor.args[0]["app"] != "photos" || observations["app.health"].(map[string]any)["ok"] != true {
		t.Fatalf("target or read result missing: args=%#v observations=%#v", executor.args, observations)
	}
}

func TestNostrOperationObserverRejectsNonReadOperations(t *testing.T) {
	executor := &recordingOperationExecutor{}
	if _, err := NewNostrOperationObserver(executor, nil, []ObservationQuery{{
		Operation: "service.restart", Args: map[string]any{"name": "web"},
	}}); err == nil {
		t.Fatal("write operation accepted as an observation source")
	}
	if _, err := NewNostrOperationObserver(executor, nil, []ObservationQuery{{Operation: "shell.run"}}); err == nil {
		t.Fatal("unregistered observation operation accepted")
	}
}

func TestNostrOperationObserverRecordsReadFailuresWithoutLeakingErrors(t *testing.T) {
	executor := &recordingOperationExecutor{
		results:  map[string]map[string]any{"disk.status": {"free_bytes": 100}},
		failures: map[string]error{"app.health": errors.New("secret-bearing adapter detail")},
	}
	observer, err := NewNostrOperationObserver(executor, nil, []ObservationQuery{
		{Operation: "app.health", TargetArg: "app"},
		{Operation: "disk.status"},
	})
	if err != nil {
		t.Fatal(err)
	}
	observations, err := observer.Observe(context.Background(), "health_check_failed", "photos")
	if err != nil {
		t.Fatal(err)
	}
	failure := observations["app.health"].(map[string]any)
	if failure["error"] != "read_failed" || failure["ok"] != false {
		t.Fatalf("failed read was not safely represented: %#v", failure)
	}
	if _, leaked := failure["detail"]; leaked {
		t.Fatalf("adapter error detail leaked to observations: %#v", failure)
	}
	if observations["disk.status"].(map[string]any)["ok"] != true {
		t.Fatalf("a failed query prevented remaining observations: %#v", observations)
	}
}
