// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

func twoFileSet(t *testing.T) gitdiff.ChangeSet {
	t.Helper()
	set := testChangeSet(t, "z.go")
	first := copyChangeSet(set).Files[0]
	first.Path = "a.go"
	set.Files = append(set.Files, first)
	return set
}

func findingCall(path, id string) llm.ToolCall {
	call := commentCall(id)
	call.Arguments = json.RawMessage(strings.Replace(string(call.Arguments), "sample.go", path, 1))
	return call
}

func fileOutcome(t *testing.T, result ReviewResult, path string) model.FileResult {
	t.Helper()
	for _, file := range result.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("missing file outcome for %s: %+v", path, result.Files)
	return model.FileResult{}
}

func TestAgentSerialOrderFreshHistoryAndBoundRegistries(t *testing.T) {
	set := twoFileSet(t)
	set.Warnings = []string{"Preserved input warning."}
	originalPaths := []string{}
	for _, file := range set.Files {
		originalPaths = append(originalPaths, file.Path)
	}
	steps := []llm.MockStep{
		scriptStep(testCall("read_a", tool.FileRead, `{"path":"related.go"}`)),
		scriptStep(findingCall("a.go", "first_a"), findingCall("a.go", "duplicate_a"), doneCall("done_a")),
		scriptStep(findingCall("a.go", "wrong_current"), findingCall("z.go", "first_z"), doneCall("rejected_done_z")),
		scriptStep(doneCall("renewed_done_z")),
	}
	zero := int64(0)
	for i := range steps {
		input := int64(i + 1)
		steps[i].Response.Usage = &llm.TokenUsage{InputTokens: &input, OutputTokens: &zero}
	}
	client := llm.NewMockClient(steps)
	reads := 0
	agent, err := NewAgent(client, RunnerOptions{}, func(_ context.Context, snapshot gitdiff.Snapshot, path string) ([]byte, error) {
		reads++
		if snapshot != set.Snapshot || path != "related.go" {
			t.Fatalf("reader lost fixed snapshot: %+v %s", snapshot, path)
		}
		return []byte("package related\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), set, "Verify related context.")
	if err != nil || reads != 1 || len(result.Findings) != 2 || result.HasCoverageGap || len(result.FileErrors) != 0 || result.Snapshot != set.Snapshot {
		t.Fatalf("serial review failed: %+v %v", result, err)
	}
	if result.Findings[0].Path != "a.go" || result.Findings[1].Path != "z.go" || result.Usage.LLMRequests != 4 || result.Usage.ToolCalls != 8 || *result.Usage.InputTokens != 10 || *result.Usage.OutputTokens != 0 {
		t.Fatalf("wrong findings or aggregate usage: %+v", result)
	}
	paths := []string{}
	for _, file := range result.Files {
		paths = append(paths, file.Path)
	}
	if !reflect.DeepEqual(paths, []string{"a.go", "deleted.go", "notes.md", "z.go"}) {
		t.Fatalf("tasks not sorted: %v", paths)
	}
	for _, path := range []string{"a.go", "z.go"} {
		if file := fileOutcome(t, result, path); file.Status != model.FileCompleted || file.Rounds != 2 || file.StopReason != "task_done" {
			t.Fatalf("wrong file completion: %+v", file)
		}
	}
	requests := client.Requests()
	for _, index := range []int{0, 2} {
		if len(requests[index].Messages) != 2 {
			t.Fatal("new file retained previous history")
		}
		var data struct {
			Path  string   `json:"current_path"`
			Paths []string `json:"changed_paths"`
		}
		if err := json.Unmarshal([]byte(requests[index].Messages[1].Content), &data); err != nil {
			t.Fatal(err)
		}
		want := "a.go"
		if index == 2 {
			want = "z.go"
		}
		if data.Path != want || !reflect.DeepEqual(data.Paths, paths) {
			t.Fatalf("incorrect current task or complete path list: %+v", data)
		}
	}
	assertToolMessage(t, requests[3].Messages[3], "wrong_current", "wrong_file")
	for i, file := range set.Files {
		if file.Path != originalPaths[i] {
			t.Fatal("sorting mutated input change set")
		}
	}
	result.Warnings[0] = "changed result warning"
	if set.Warnings[0] != "Preserved input warning." {
		t.Fatal("result retained caller warning slice")
	}
}

func TestAgentLocalFailuresContinueAndKeepEarlierFindings(t *testing.T) {
	for _, reason := range []string{"model_error", "request_timeout", "tool_timeout"} {
		t.Run(reason, func(t *testing.T) {
			set := twoFileSet(t)
			cause := errors.New("fixture model failure")
			steps := []llm.MockStep{scriptStep(findingCall("a.go", "accepted_a")), scriptStep(findingCall("z.go", "accepted_z"), doneCall("done_z"))}
			if reason == "tool_timeout" {
				steps = []llm.MockStep{steps[0], scriptStep(testCall("timeout_read", tool.FileRead, `{"path":"related.go"}`), doneCall("never_a")), steps[1]}
			}
			mock := llm.NewMockClient(steps)
			calls := 0
			client := testClient(func(ctx context.Context, request llm.Request) (llm.Response, error) {
				calls++
				if calls == 2 && reason != "tool_timeout" {
					if reason == "request_timeout" {
						<-ctx.Done()
						return llm.Response{}, ctx.Err()
					}
					return llm.Response{}, cause
				}
				return mock.Complete(ctx, request)
			})
			agent, err := NewAgent(client, RunnerOptions{RequestTimeout: 20 * time.Millisecond}, func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
				return nil, fmt.Errorf("fixture Git timeout: %w", context.DeadlineExceeded)
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := agent.Run(context.Background(), set, "")
			if err != nil || calls != 3 || !result.HasCoverageGap || len(result.Findings) != 2 || result.Usage.LLMRequests != 3 {
				t.Fatalf("local failure aborted or retried review: %+v %v", result, err)
			}
			first, second := fileOutcome(t, result, "a.go"), fileOutcome(t, result, "z.go")
			if first.Status != model.FileFailed || first.StopReason != reason || first.Rounds != 2 || second.Status != model.FileCompleted || second.Rounds != 1 {
				t.Fatalf("incorrect failure isolation: %+v %+v", first, second)
			}
			want := error(context.DeadlineExceeded)
			if reason == "model_error" {
				want = cause
			}
			if len(result.FileErrors) != 1 || !errors.Is(result.FileErrors["a.go"], want) {
				t.Fatalf("local cause was lost: %v", result.FileErrors)
			}
		})
	}
}

func TestAgentNormalStopsContinueWithoutGoErrors(t *testing.T) {
	for _, reason := range []string{"invalid_response", "max_rounds", "context_limit"} {
		t.Run(reason, func(t *testing.T) {
			set := twoFileSet(t)
			options := RunnerOptions{}
			steps := []llm.MockStep{}
			switch reason {
			case "invalid_response":
				steps = append(steps, scriptStep(testCall("bad_1", "unknown", `{}`)), scriptStep(testCall("bad_2", "unknown", `{}`)))
			case "max_rounds":
				options.MaxRounds = 1
				steps = append(steps, scriptStep(findingCall("a.go", "accepted_a")))
			case "context_limit":
				steps = append(steps, scriptStep(testCall("bad", "unknown", strings.Repeat("x", 65000))))
			}
			steps = append(steps, scriptStep(doneCall("done_z")))
			mock := llm.NewMockClient(steps)
			agent, _ := NewAgent(mock, options, nil)
			result, err := agent.Run(context.Background(), set, "")
			if err != nil || fileOutcome(t, result, "a.go").StopReason != reason || fileOutcome(t, result, "z.go").Status != model.FileCompleted || len(result.FileErrors) != 0 || !result.HasCoverageGap {
				t.Fatalf("normal stop aborted other files: %+v %v", result, err)
			}
		})
	}
}

func TestAgentParentCancellationStopsPendingFiles(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		set := twoFileSet(t)
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
		}
		calls := 0
		client := testClient(func(requestCtx context.Context, _ llm.Request) (llm.Response, error) {
			calls++
			if calls == 1 {
				return scriptStep(findingCall("a.go", "accepted")).Response, nil
			}
			if deadline {
				<-requestCtx.Done()
			} else {
				cancel()
			}
			return llm.Response{}, requestCtx.Err()
		})
		agent, _ := NewAgent(client, RunnerOptions{RequestTimeout: time.Second}, nil)
		result, err := agent.Run(ctx, set, "")
		want := error(context.Canceled)
		if deadline {
			want = context.DeadlineExceeded
		}
		cancel()
		if !errors.Is(err, want) || calls != 2 || result.Usage.LLMRequests != 2 || len(result.Findings) != 1 || !result.HasCoverageGap {
			t.Fatalf("parent cancel did not stop scheduler: %+v %v", result, err)
		}
		if first, pending := fileOutcome(t, result, "a.go"), fileOutcome(t, result, "z.go"); first.StopReason != "canceled" || first.Rounds != 2 || pending.Status != model.FileFailed || pending.StopReason != "canceled" || pending.Rounds != 0 {
			t.Fatalf("pending file was dispatched or marked skipped: %+v %+v", first, pending)
		}
		if skipped := fileOutcome(t, result, "notes.md"); skipped.Status != model.FileSkipped || skipped.StopReason != gitdiff.SkipUnsupported {
			t.Fatal("cancellation overwrote static skip")
		}
	}
}

func TestAgentEmptyAndExcludedInputsNeverRequestModel(t *testing.T) {
	for _, empty := range []bool{false, true} {
		set := testChangeSet(t, "sample.go")
		set.Files = set.Files[1:]
		set.Warnings = []string{"Input coverage warning."}
		set.HasCoverageGap = true
		if empty {
			set.Files = nil
		}
		mock := llm.NewMockClient(nil)
		agent, _ := NewAgent(mock, RunnerOptions{}, nil)
		result, err := agent.Run(context.Background(), set, "")
		if err != nil || len(mock.Requests()) != 0 || result.Usage.LLMRequests != 0 || result.Usage.ToolCalls != 0 || *result.Usage.InputTokens != 0 || *result.Usage.OutputTokens != 0 || len(result.Findings) != 0 || result.Findings == nil || result.Files == nil || !result.HasCoverageGap || !reflect.DeepEqual(result.Warnings, set.Warnings) {
			t.Fatalf("empty/skipped input lost coverage or invented usage: %+v %v", result, err)
		}
		for _, file := range result.Files {
			if file.Status != model.FileSkipped || file.Rounds != 0 {
				t.Fatalf("static exclusion was executed: %+v", file)
			}
		}
	}
}

func TestAgentRepeatedRunsHaveSeparateCollectorsAndInputCopies(t *testing.T) {
	set := twoFileSet(t)
	mock := llm.NewMockClient([]llm.MockStep{
		scriptStep(findingCall("a.go", "a1"), doneCall("done_a1")), scriptStep(findingCall("z.go", "z1"), doneCall("done_z1")),
		scriptStep(findingCall("a.go", "a2"), doneCall("done_a2")), scriptStep(findingCall("z.go", "z2"), doneCall("done_z2")),
	})
	client := testClient(func(ctx context.Context, request llm.Request) (llm.Response, error) {
		if len(mock.Requests()) == 0 {
			set.Files[0].HeadContent[0] = 0
			delete(set.Files[0].AddedLines, 5)
		}
		return mock.Complete(ctx, request)
	})
	agent, _ := NewAgent(client, RunnerOptions{}, nil)
	first, err := agent.Run(context.Background(), set, "")
	if err != nil || len(first.Findings) != 2 || first.HasCoverageGap {
		t.Fatalf("caller mutation changed prepared tasks: %+v %v", first, err)
	}
	set = twoFileSet(t)
	first.Findings[0].Title = "changed returned finding"
	second, err := agent.Run(context.Background(), set, "")
	if err != nil || len(second.Findings) != 2 || second.Findings[0].Title != "Integer sum may overflow" || second.Usage.LLMRequests != 2 {
		t.Fatalf("runs shared collector or counters: %+v %v", second, err)
	}
}

func TestAgentRejectsBadConfigurationAndRetainsCanceledMetadata(t *testing.T) {
	mock := llm.NewMockClient(nil)
	if _, err := NewAgent(nil, RunnerOptions{}, nil); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := NewAgent(mock, RunnerOptions{MaxRounds: 21}, nil); err == nil {
		t.Fatal("invalid options accepted")
	}
	agent, _ := NewAgent(mock, RunnerOptions{}, nil)
	set := twoFileSet(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := agent.Run(ctx, set, "")
	if !errors.Is(err, context.Canceled) || len(result.Files) != len(set.Files) || len(mock.Requests()) != 0 || *result.Usage.InputTokens != 0 || fileOutcome(t, result, "a.go").StopReason != "canceled" {
		t.Fatalf("pre-canceled review lost input outcomes: %+v %v", result, err)
	}
	set.Snapshot.HeadSHA = "HEAD"
	if _, err := agent.Run(context.Background(), set, ""); err == nil {
		t.Fatal("unpinned snapshot accepted")
	}
	set = twoFileSet(t)
	set.Files = append(set.Files, set.Files[0])
	if _, err := agent.Run(context.Background(), set, ""); err == nil || len(mock.Requests()) != 0 {
		t.Fatal("duplicate task was dispatched")
	}
}

func TestAgentUsageAggregatesUnknownFieldsIndependently(t *testing.T) {
	two, three, zero := int64(2), int64(3), int64(0)
	for _, tc := range []struct {
		name          string
		first, second *llm.TokenUsage
		input, output *int64
	}{
		{"unknown input", &llm.TokenUsage{OutputTokens: &two}, &llm.TokenUsage{InputTokens: &three, OutputTokens: &zero}, nil, &two},
		{"unknown output", &llm.TokenUsage{InputTokens: &two}, &llm.TokenUsage{InputTokens: &zero, OutputTokens: &three}, &two, nil},
		{"missing usage", nil, &llm.TokenUsage{InputTokens: &zero, OutputTokens: &zero}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []llm.MockStep{scriptStep(doneCall("a")), scriptStep(doneCall("z"))}
			steps[0].Response.Usage, steps[1].Response.Usage = tc.first, tc.second
			agent, _ := NewAgent(llm.NewMockClient(steps), RunnerOptions{}, nil)
			result, err := agent.Run(context.Background(), twoFileSet(t), "")
			usage := result.Usage
			if err != nil || usage.LLMRequests != 2 || usage.ToolCalls != 2 ||
				(tc.input == nil) != (usage.InputTokens == nil) || (tc.input != nil && *tc.input != *usage.InputTokens) ||
				(tc.output == nil) != (usage.OutputTokens == nil) || (tc.output != nil && *tc.output != *usage.OutputTokens) {
				t.Fatalf("aggregate usage changed unknown/zero values: %+v %v", usage, err)
			}
		})
	}
}
