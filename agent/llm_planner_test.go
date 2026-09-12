package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func plannerResponse(status int, body string, request *http.Request) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func TestOpenAICompatiblePlannerSendsOnlyGrantedToolsAndParsesToolCalls(t *testing.T) {
	var got completionRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected endpoint path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer local-key" {
			t.Errorf("local API key header missing")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode completion request: %v", err)
		}
		return plannerResponse(http.StatusOK, `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"app.health","arguments":"{\"app\":\"photos\"}"}}]}}]}`, r), nil
	})}

	planner, err := NewOpenAICompatiblePlanner(LLMPlannerConfig{BaseURL: "http://127.0.0.1:8080/v1", Model: "qwen-local", APIKey: "local-key", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	operations := []OperationSpec{DefaultRegistry()["app.health"]}
	proposals, err := planner.Plan(context.Background(), PlanningInput{
		Trigger: "health_check_failed", Target: "photos",
		Observations: map[string]any{"app": "photos", "health": "failed"}, Operations: operations,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].Operation != "app.health" || proposals[0].Args["app"] != "photos" {
		t.Fatalf("unexpected proposal output: %#v", proposals)
	}
	if got.Model != "qwen-local" || got.ToolChoice != "auto" || got.MaxTokens != maxPlannerOutputTokens || got.ReasoningEffort != "none" || len(got.Tools) != 1 || got.Tools[0].Function.Name != "app.health" {
		t.Fatalf("planner request did not preserve the host tool boundary: %#v", got)
	}
	if !strings.Contains(string(got.Tools[0].Function.Parameters), `"required":["app"]`) {
		t.Fatalf("strict argument schema missing from tool definition: %s", got.Tools[0].Function.Parameters)
	}
}

func TestOpenAICompatiblePlannerRejectsRemoteEndpointsAndDoesNotFollowRedirects(t *testing.T) {
	if _, err := NewOpenAICompatiblePlanner(LLMPlannerConfig{BaseURL: "https://inference.example.com/v1", Model: "model"}); err == nil {
		t.Fatal("remote inference endpoint accepted")
	}
	remoteCalled := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "127.0.0.1:8080" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		remoteCalled = true
		return plannerResponse(http.StatusOK, `{}`, r), nil
	})}
	planner, err := NewOpenAICompatiblePlanner(LLMPlannerConfig{BaseURL: "http://127.0.0.1:8080", Model: "model", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = planner.Plan(context.Background(), PlanningInput{Operations: []OperationSpec{DefaultRegistry()["app.health"]}})
	if err == nil || remoteCalled {
		t.Fatalf("planner followed local endpoint redirect: err=%v remote_called=%v", err, remoteCalled)
	}
}

func TestOpenAICompatiblePlannerHandlesNoToolCallAndNoGrantedTools(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return plannerResponse(http.StatusOK, `{"choices":[{"message":{"content":"insufficient evidence"}}]}`, r), nil
	})}
	planner, err := NewOpenAICompatiblePlanner(LLMPlannerConfig{BaseURL: "http://127.0.0.1:8080", Model: "model", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := planner.Plan(context.Background(), PlanningInput{})
	if err != nil || proposals != nil || requests != 0 {
		t.Fatalf("planner contacted model without granted tools: proposals=%#v requests=%d err=%v", proposals, requests, err)
	}
	proposals, err = planner.Plan(context.Background(), PlanningInput{Operations: []OperationSpec{DefaultRegistry()["app.health"]}})
	if err != nil || len(proposals) != 0 || requests != 1 {
		t.Fatalf("no-tool response mishandled: proposals=%#v requests=%d err=%v", proposals, requests, err)
	}
}

func TestOpenAICompatiblePlannerRejectsMalformedToolArguments(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return plannerResponse(http.StatusOK, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"app.health","arguments":"[]"}}]}}]}`, r), nil
	})}
	planner, err := NewOpenAICompatiblePlanner(LLMPlannerConfig{BaseURL: "http://127.0.0.1:8080", Model: "model", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Plan(context.Background(), PlanningInput{Operations: []OperationSpec{DefaultRegistry()["app.health"]}}); err == nil {
		t.Fatal("non-object tool arguments accepted")
	}
}
