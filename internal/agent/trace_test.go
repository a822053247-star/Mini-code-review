// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/trace"
)

func TestAgentTraceFailureStopsAndPreservesFindings(t *testing.T) {
	set := twoFileSet(t)
	mock := llm.NewMockClient([]llm.MockStep{scriptStep(findingCall("a.go", "accepted"), doneCall("never")), scriptStep(doneCall("never_z"))})
	events := []trace.Event{}
	sink := func(event trace.Event) error {
		events = append(events, event)
		if event.Event == "tool_end" && event.ToolCallID == "accepted" {
			return errors.New("private trace error")
		}
		return nil
	}
	agent, err := NewAgent(mock, RunnerOptions{Events: sink}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), set, "")
	if !trace.IsError(err) || len(result.Findings) != 1 || result.Usage.ToolCalls != 1 || len(mock.Requests()) != 1 || fileOutcome(t, result, "z.go").Rounds != 0 || fileOutcome(t, result, "a.go").StopReason != "trace_error" {
		t.Fatalf("trace failure lost accepted work or continued: %+v %v", result, err)
	}
	if strings.Contains(err.Error(), "private trace error") {
		t.Fatal("trace diagnostics leaked writer data")
	}
}

func TestRunnerTraceAssociatesCallsAndActualUsage(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	zero := int64(0)
	steps := []llm.MockStep{scriptStep(doneCall("done"))}
	steps[0].Response.Usage = &llm.TokenUsage{InputTokens: &zero}
	var buffer bytes.Buffer
	logger, _ := trace.New(&buffer)
	runner, _ := NewRunner(llm.NewMockClient(steps), registry, RunnerOptions{Events: logger.Record})
	result, _, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.Status != model.FileCompleted {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 4 {
		t.Fatal("missing request/tool start and end events")
	}
	var requestEnd, toolEnd trace.Event
	json.Unmarshal([]byte(lines[1]), &requestEnd)
	json.Unmarshal([]byte(lines[3]), &toolEnd)
	if requestEnd.Usage == nil || requestEnd.Usage.InputTokens == nil || *requestEnd.Usage.InputTokens != 0 || requestEnd.Usage.OutputTokens != nil || toolEnd.ToolCallID != "done" || toolEnd.Tool != "task_done" || toolEnd.Round != 1 || toolEnd.DurationMS == nil {
		t.Fatal("trace lost association, timing, or actual usage")
	}
}

func TestTraceStartFailureDoesNotExecuteCalls(t *testing.T) {
	for _, eventName := range []string{"llm_start", "tool_start"} {
		registry, collector, initial := testRegistry(t, nil)
		mock := llm.NewMockClient([]llm.MockStep{scriptStep(commentCall("never"), doneCall("never_done"))})
		sink := func(event trace.Event) error {
			if event.Event == eventName {
				return errors.New("writer failure")
			}
			return nil
		}
		runner, _ := NewRunner(mock, registry, RunnerOptions{Events: sink})
		result, usage, err := runner.Run(context.Background(), "sample.go", initial)
		wantRequests := 0
		if eventName == "tool_start" {
			wantRequests = 1
		}
		if !trace.IsError(err) || result.StopReason != "trace_error" || usage.ToolCalls != 0 || usage.LLMRequests != wantRequests || len(mock.Requests()) != wantRequests || len(collector.Findings()) != 0 {
			t.Fatal("trace start failure executed or counted a blocked call")
		}
	}
}

func TestTraceUsesSafeHTTPCodeAndStatus(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	client := testClient(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{}, &llm.HTTPError{Code: "authentication_failed", StatusCode: 401}
	})
	var buffer bytes.Buffer
	logger, _ := trace.New(&buffer)
	runner, _ := NewRunner(client, registry, RunnerOptions{Events: logger.Record})
	result, _, err := runner.Run(context.Background(), "sample.go", initial)
	if err == nil || result.StopReason != "model_error" {
		t.Fatal("HTTP error did not stop file")
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	var event trace.Event
	json.Unmarshal([]byte(lines[1]), &event)
	if event.Code != "authentication_failed" || event.HTTPStatus != 401 {
		t.Fatal("trace lost safe HTTP error classification")
	}
}
