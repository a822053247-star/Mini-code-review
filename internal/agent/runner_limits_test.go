// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

func TestRunnerDefaultAndRoundLimitBoundaries(t *testing.T) {
	for _, limit := range []int{0, 1, 20} {
		for _, complete := range []bool{false, true} {
			maxRounds := limit
			if maxRounds == 0 {
				maxRounds = 8
			}
			steps := make([]llm.MockStep, maxRounds+1)
			for i := range steps {
				steps[i] = scriptStep(testCall(fmt.Sprint(i), tool.FileRead, `{"path":"sample.go"}`))
			}
			if complete {
				steps[maxRounds-1] = scriptStep(doneCall("last_done"))
			}
			result, usage, client, _ := runScript(t, steps, RunnerOptions{MaxRounds: limit})
			want := "max_rounds"
			if complete {
				want = "task_done"
			}
			if result.StopReason != want || result.Rounds != maxRounds || usage.LLMRequests != maxRounds || len(client.Requests()) != maxRounds {
				t.Fatalf("round boundary %d complete=%t: %+v %+v", limit, complete, result, usage)
			}
		}
	}
}

func TestRunnerExact64KiBUTF8Budget(t *testing.T) {
	registry, _, _ := testRegistry(t, nil)
	for _, size := range []int{65536, 65537} {
		initial := []llm.Message{{Role: llm.RoleSystem, Content: "S"}, {Role: llm.RoleUser, Content: strings.Repeat("x", size-4) + "界"}}
		client := llm.NewMockClient([]llm.MockStep{scriptStep(doneCall("done"))})
		runner, err := NewRunner(client, registry, RunnerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		result, usage, err := runner.Run(context.Background(), "sample.go", initial)
		if err != nil {
			t.Fatal(err)
		}
		if size == 65536 {
			if result.Status != model.FileCompleted || usage.LLMRequests != 1 {
				t.Fatalf("exact budget rejected: %+v", result)
			}
		} else if result.StopReason != "context_limit" || usage.LLMRequests != 0 || len(client.Requests()) != 0 {
			t.Fatalf("oversized context reached client: %+v", result)
		}
	}
	if _, err := NewRunner(llm.NewMockClient(nil), registry, RunnerOptions{MaxContextBytes: 65537}); err == nil {
		t.Fatal("context option bypassed 64 KiB cap")
	}
	if _, err := NewRunner(llm.NewMockClient(nil), registry, RunnerOptions{RequestTimeout: 61 * time.Second}); err == nil {
		t.Fatal("timeout option bypassed 60 second cap")
	}
}

func TestRunnerCountsFullToolHistoryBeforeNextRequest(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	steps := []llm.MockStep{scriptStep(testCall("read", tool.FileRead, ` {"path":"related.go"} `)), scriptStep(doneCall("done"))}
	baseline := llm.NewMockClient(steps)
	runner, _ := NewRunner(baseline, registry, RunnerOptions{})
	if _, _, err := runner.Run(context.Background(), "sample.go", initial); err != nil {
		t.Fatal(err)
	}
	history := baseline.Requests()[1].Messages
	bytes := 0
	for _, message := range history {
		bytes += len(message.Content)
		for _, call := range message.ToolCalls {
			bytes += len(call.Arguments)
		}
	}
	for _, extra := range []int{0, 1} {
		padded := copyMessages(initial)
		padded[1].Content += strings.Repeat("x", 65536-bytes+extra)
		client := llm.NewMockClient(steps)
		runner, _ := NewRunner(client, registry, RunnerOptions{})
		result, _, err := runner.Run(context.Background(), "sample.go", padded)
		if err != nil {
			t.Fatal(err)
		}
		if extra == 0 && (result.Status != model.FileCompleted || len(client.Requests()) != 2) {
			t.Fatalf("exact cumulative budget rejected: %+v", result)
		}
		if extra == 1 && (result.StopReason != "context_limit" || len(client.Requests()) != 1) {
			t.Fatalf("tool history not counted: %+v", result)
		}
	}
}

func TestRunnerInvalidRoundKindsShareCounterAndPrecedeLimits(t *testing.T) {
	text := llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("x", 40000)}}}
	invalid := scriptStep(doneCall(""))
	failed := scriptStep(testCall("unknown", "unknown", `{}`))
	for _, first := range []llm.MockStep{text, invalid, failed} {
		for _, second := range []llm.MockStep{text, invalid, failed} {
			result, _, client, _ := runScript(t, []llm.MockStep{first, second, scriptStep(doneCall("never"))}, RunnerOptions{MaxRounds: 2})
			if result.StopReason != "invalid_response" || result.Rounds != 2 || len(client.Requests()) != 2 {
				t.Fatalf("invalid counter or precedence changed: %+v", result)
			}
		}
	}
	result, _, _, _ := runScript(t, []llm.MockStep{failed, scriptStep(testCall("read", tool.FileRead, `{"path":"related.go"}`)), failed, scriptStep(doneCall("done"))}, RunnerOptions{})
	if result.Status != model.FileCompleted || result.Rounds != 4 {
		t.Fatal("successful read did not reset invalid counter")
	}
}

func TestRunnerExhaustedRoundsPrecedeNextContextCheck(t *testing.T) {
	registry, collector, initial := testRegistry(t, nil)
	budget := len(initial[0].Content) + len(initial[1].Content)
	client := llm.NewMockClient([]llm.MockStep{scriptStep(commentCall("accepted"))})
	runner, _ := NewRunner(client, registry, RunnerOptions{MaxRounds: 1, MaxContextBytes: budget})
	result, _, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.StopReason != "max_rounds" || len(collector.Findings()) != 1 {
		t.Fatalf("round/context precedence changed: %+v %v", result, err)
	}
}

func TestRunnerRequestContextsAreReleasedBetweenRounds(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	var previous context.Context
	count := 0
	client := testClient(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		if previous != nil && !errors.Is(previous.Err(), context.Canceled) {
			t.Fatal("previous request context was not released")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 60*time.Second || ctx.Err() != nil {
			t.Fatal("request context has no active 60 second bound")
		}
		previous = ctx
		count++
		if count == 1 {
			return scriptStep(testCall("read", tool.FileRead, `{"path":"related.go"}`)).Response, nil
		}
		return scriptStep(doneCall("done")).Response, nil
	})
	runner, _ := NewRunner(client, registry, RunnerOptions{})
	if result, _, err := runner.Run(context.Background(), "sample.go", initial); err != nil || result.Status != model.FileCompleted {
		t.Fatalf("request lifecycle failed: %+v %v", result, err)
	}
	if previous.Err() != context.Canceled {
		t.Fatal("final request context was not released")
	}
}

func TestRunnerFailuresAndToolDeadlinePreserveAcceptedFindings(t *testing.T) {
	for _, kind := range []string{"model_error", "request_timeout", "tool_timeout"} {
		t.Run(kind, func(t *testing.T) {
			var read tool.HeadFileReader
			if kind == "tool_timeout" {
				read = func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
					return nil, fmt.Errorf("fixture Git timeout: %w", context.DeadlineExceeded)
				}
			}
			registry, collector, initial := testRegistry(t, read)
			calls := 0
			cause := errors.New("fixture model failure")
			client := testClient(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
				calls++
				if calls == 1 {
					return scriptStep(commentCall("accepted")).Response, nil
				}
				if kind == "tool_timeout" {
					return scriptStep(testCall("timed_read", tool.FileRead, `{"path":"related.go"}`), doneCall("never")).Response, nil
				}
				if kind == "request_timeout" {
					<-ctx.Done()
					return llm.Response{}, ctx.Err()
				}
				return llm.Response{}, cause
			})
			runner, _ := NewRunner(client, registry, RunnerOptions{RequestTimeout: 20 * time.Millisecond})
			result, usage, err := runner.Run(context.Background(), "sample.go", initial)
			want := error(context.DeadlineExceeded)
			if kind == "model_error" {
				want = cause
			}
			wantTools := 1
			if kind == "tool_timeout" {
				wantTools = 2
			}
			if !errors.Is(err, want) || result.StopReason != kind || result.Rounds != 2 || usage.ToolCalls != wantTools || calls != 2 || len(collector.Findings()) != 1 {
				t.Fatalf("failed stop lost state or retried: %+v %+v %v", result, usage, err)
			}
		})
	}
}

func TestRunnerParentCancellationPrecedesLocalFailures(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
		}
		client := testClient(func(requestCtx context.Context, _ llm.Request) (llm.Response, error) {
			if deadline {
				<-requestCtx.Done()
			} else {
				cancel()
			}
			return scriptStep(doneCall("never")).Response, errors.New("local model failure")
		})
		runner, _ := NewRunner(client, registry, RunnerOptions{RequestTimeout: time.Second})
		result, usage, err := runner.Run(ctx, "sample.go", initial)
		want := error(context.Canceled)
		if deadline {
			want = context.DeadlineExceeded
		}
		cancel()
		if !errors.Is(err, want) || result.StopReason != "canceled" || usage.ToolCalls != 0 {
			t.Fatalf("parent cause lost: %+v %+v %v", result, usage, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := llm.NewMockClient(nil)
	runner, _ := NewRunner(client, registry, RunnerOptions{MaxRounds: 1, MaxContextBytes: 1})
	result, usage, err := runner.Run(ctx, "sample.go", initial)
	if !errors.Is(err, context.Canceled) || result.StopReason != "canceled" || usage.LLMRequests != 0 || *usage.InputTokens != 0 || len(client.Requests()) != 0 {
		t.Fatal("initial cancellation did not precede limits")
	}
}

func TestRunnerUsageUnknownFieldsAndOverflow(t *testing.T) {
	zero, two, three, max := int64(0), int64(2), int64(3), int64(math.MaxInt64)
	for _, tc := range []struct {
		name          string
		first, second *llm.TokenUsage
		input, output *int64
	}{
		{"known", &llm.TokenUsage{InputTokens: &two, OutputTokens: &zero}, &llm.TokenUsage{InputTokens: &three, OutputTokens: &zero}, nil, &zero},
		{"unknown input", &llm.TokenUsage{OutputTokens: &two}, &llm.TokenUsage{InputTokens: &three, OutputTokens: &zero}, nil, &two},
		{"unknown output", &llm.TokenUsage{InputTokens: &two}, &llm.TokenUsage{InputTokens: &zero, OutputTokens: &three}, &two, nil},
		{"missing usage", nil, &llm.TokenUsage{InputTokens: &two, OutputTokens: &zero}, nil, nil},
		{"overflow", &llm.TokenUsage{InputTokens: &max, OutputTokens: &zero}, &llm.TokenUsage{InputTokens: &two, OutputTokens: &zero}, nil, &zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []llm.MockStep{scriptStep(testCall("read", tool.FileRead, `{"path":"related.go"}`)), scriptStep(doneCall("done"))}
			steps[0].Response.Usage, steps[1].Response.Usage = tc.first, tc.second
			_, usage, _, _ := runScript(t, steps, RunnerOptions{})
			wantInput := tc.input
			five := int64(5)
			if tc.name == "known" {
				wantInput = &five
			}
			if (wantInput == nil) != (usage.InputTokens == nil) || (wantInput != nil && *wantInput != *usage.InputTokens) ||
				(tc.output == nil) != (usage.OutputTokens == nil) || (tc.output != nil && *tc.output != *usage.OutputTokens) {
				t.Fatalf("usage unknown/zero contract changed: %+v", usage)
			}
		})
	}
	registry, _, initial := testRegistry(t, nil)
	cause := errors.New("request failed after reporting usage")
	client := testClient(func(context.Context, llm.Request) (llm.Response, error) {
		return llm.Response{Usage: &llm.TokenUsage{InputTokens: &two}}, cause
	})
	runner, _ := NewRunner(client, registry, RunnerOptions{})
	result, usage, err := runner.Run(context.Background(), "sample.go", initial)
	if !errors.Is(err, cause) || result.StopReason != "model_error" || usage.InputTokens == nil || *usage.InputTokens != two || usage.OutputTokens != nil {
		t.Fatalf("reported usage was discarded on request failure: %+v %+v %v", result, usage, err)
	}
}
