// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

type testClient func(context.Context, llm.Request) (llm.Response, error)

func (client testClient) Complete(ctx context.Context, request llm.Request) (llm.Response, error) {
	return client(ctx, request)
}

func testRegistry(t *testing.T, read tool.HeadFileReader) (*tool.Registry, *tool.Collector, []llm.Message) {
	t.Helper()
	set := testChangeSet(t, "sample.go")
	if read == nil {
		read = func(_ context.Context, _ gitdiff.Snapshot, path string) ([]byte, error) {
			switch path {
			case "sample.go":
				return []byte(testHead), nil
			case "related.go":
				return []byte("package sample\nconst MaxInput = 100\n"), nil
			default:
				return nil, errors.New("fixture file missing")
			}
		}
	}
	reader, err := tool.NewFileReader(set.Snapshot, read)
	if err != nil {
		t.Fatal(err)
	}
	collector := tool.NewCollector()
	commenter, err := tool.NewCommenter(set.Files[0], collector)
	if err != nil {
		t.Fatal(err)
	}
	finisher, err := tool.NewTaskFinisher("sample.go")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry(tool.Handlers{FileRead: reader.Execute, CodeComment: commenter.Execute, TaskDone: finisher.Execute})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := BuildInitialMessages(set, "sample.go", "Check arithmetic.")
	if err != nil {
		t.Fatal(err)
	}
	return registry, collector, initial
}

func testCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}
func doneCall(id string) llm.ToolCall {
	return testCall(id, tool.TaskDone, `{"summary":"Checked arithmetic and related context."}`)
}
func commentCall(id string) llm.ToolCall {
	return testCall(id, tool.CodeComment, `{"path":"sample.go","start_line":5,"end_line":6,"severity":"low","category":"bug","title":"Integer sum may overflow","description":"Large inputs can overflow and produce an incorrect sum.","suggestion":"Check input bounds before addition."}`)
}
func scriptStep(calls ...llm.ToolCall) llm.MockStep {
	return llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}}
}
func runScript(t *testing.T, steps []llm.MockStep, options RunnerOptions) (model.FileResult, model.Usage, *llm.MockClient, *tool.Collector) {
	t.Helper()
	registry, collector, initial := testRegistry(t, nil)
	client := llm.NewMockClient(steps)
	runner, err := NewRunner(client, registry, options)
	if err != nil {
		t.Fatal(err)
	}
	result, usage, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil {
		t.Fatal(err)
	}
	return result, usage, client, collector
}

func assertToolMessage(t *testing.T, message llm.Message, id, code string) map[string]any {
	t.Helper()
	var result map[string]any
	if message.Role != llm.RoleTool || message.ToolCallID != id || len(message.ToolCalls) != 0 {
		t.Fatalf("tool result lost call association: %+v", message)
	}
	if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
		t.Fatal(err)
	}
	if _, leaked := result["Completion"]; leaked {
		t.Fatal("completion leaked to model")
	}
	if _, leaked := result["completion"]; leaked {
		t.Fatal("completion leaked to model")
	}
	if code == "" {
		if result["ok"] != true || result["error"] != nil {
			t.Fatalf("expected tool success: %+v", result)
		}
	} else if result["ok"] != false || result["error"].(map[string]any)["code"] != code || result["data"] != nil {
		t.Fatalf("unexpected tool failure: %+v", result)
	}
	return result
}

func TestRunnerReadCommentDonePreservesHistory(t *testing.T) {
	steps := []llm.MockStep{scriptStep(testCall("read", tool.FileRead, ` {"path":"related.go"} `)), scriptStep(commentCall("comment")), scriptStep(doneCall("done"))}
	input, output := int64(2), int64(0)
	for i := range steps {
		steps[i].Response.Usage = &llm.TokenUsage{InputTokens: &input, OutputTokens: &output}
	}
	result, usage, client, collector := runScript(t, steps, RunnerOptions{})
	if result != (model.FileResult{Path: "sample.go", Status: model.FileCompleted, StopReason: "task_done", Rounds: 3}) || usage.LLMRequests != 3 || usage.ToolCalls != 3 ||
		usage.InputTokens == nil || *usage.InputTokens != 6 || usage.OutputTokens == nil || *usage.OutputTokens != 0 || len(collector.Findings()) != 1 {
		t.Fatalf("unexpected completion or usage: %+v %+v", result, usage)
	}
	requests := client.Requests()
	for i, request := range requests {
		if len(request.Messages) != 2+2*i || !reflect.DeepEqual(request.Tools, requests[0].Tools) || len(request.Tools) != 3 {
			t.Fatalf("request %d lost messages or definitions: %+v", i, request)
		}
		if !reflect.DeepEqual(request.Messages[:2], requests[0].Messages) {
			t.Fatal("initial context drifted")
		}
		for j := 0; j < i; j++ {
			if !reflect.DeepEqual(request.Messages[2+2*j], steps[j].Response.Assistant) {
				t.Fatal("assistant calls or raw arguments changed")
			}
			assertToolMessage(t, request.Messages[3+2*j], steps[j].Response.Assistant.ToolCalls[0].ID, "")
		}
	}
	read := assertToolMessage(t, requests[1].Messages[3], "read", "")["data"].(map[string]any)
	if read["head_sha"] != strings.Repeat("2", 40) || read["content"] != "1|package sample\n2|const MaxInput = 100\n" {
		t.Fatalf("related context read drifted: %+v", read)
	}
}

func TestRunnerProcessesAllCallsBeforeCompleting(t *testing.T) {
	for _, doneFirst := range []bool{false, true} {
		calls := []llm.ToolCall{commentCall("first"), commentCall("duplicate"), doneCall("done")}
		if doneFirst {
			calls = []llm.ToolCall{doneCall("done"), commentCall("first"), commentCall("duplicate")}
		}
		result, usage, _, collector := runScript(t, []llm.MockStep{scriptStep(calls...)}, RunnerOptions{MaxRounds: 1})
		if result.Status != model.FileCompleted || result.Rounds != 1 || usage.ToolCalls != 3 || len(collector.Findings()) != 1 {
			t.Fatalf("same-round calls skipped: %+v %+v", result, usage)
		}
	}
	result, _, _, collector := runScript(t, []llm.MockStep{scriptStep(doneCall("zero"))}, RunnerOptions{})
	if result.Status != model.FileCompleted || len(collector.Findings()) != 0 {
		t.Fatal("zero findings prevented completion")
	}
}

func TestRunnerFailuresAreFilledAndCompletionMustBeRenewed(t *testing.T) {
	calls := []llm.ToolCall{
		doneCall("early_done"), testCall("unknown", "unknown_tool", `{}`),
		testCall("bad_json", tool.CodeComment, `{"path":`), testCall("missing", tool.FileRead, `{"path":"missing.go"}`),
		testCall("wrong_file", tool.CodeComment, strings.Replace(string(commentCall("x").Arguments), "sample.go", "related.go", 1)),
		testCall("bad_anchor", tool.CodeComment, strings.Replace(string(commentCall("x").Arguments), `"start_line":5`, `"start_line":3`, 1)),
		commentCall("accepted"), commentCall("duplicate"),
	}
	steps := []llm.MockStep{scriptStep(calls...), scriptStep(testCall("read_again", tool.FileRead, `{"path":"sample.go"}`)), scriptStep(doneCall("renewed_done"))}
	result, usage, client, collector := runScript(t, steps, RunnerOptions{})
	if result.Status != model.FileCompleted || result.Rounds != 3 || usage.ToolCalls != 10 || len(collector.Findings()) != 1 {
		t.Fatalf("wrong recovery outcome: %+v %+v", result, usage)
	}
	messages := client.Requests()[1].Messages
	if len(messages) != 11 || !reflect.DeepEqual(messages[2], steps[0].Response.Assistant) {
		t.Fatal("failed round lost original assistant or tool results")
	}
	codes := []string{"", "unknown_tool", "invalid_arguments", "read_failed", "wrong_file", "invalid_anchor", "", ""}
	for i, call := range calls {
		assertToolMessage(t, messages[3+i], call.ID, codes[i])
	}
	duplicate := assertToolMessage(t, messages[10], "duplicate", "")["data"].(map[string]any)
	if duplicate["duplicate"] != true {
		t.Fatal("duplicate result not filled")
	}
	if len(client.Requests()) != 3 {
		t.Fatal("rejected completion carried into the next round")
	}
}

func TestRunnerPlainTextIsPreservedAndDoesNotComplete(t *testing.T) {
	text := llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, Content: "No issues found."}}}
	result, _, client, _ := runScript(t, []llm.MockStep{text, scriptStep(doneCall("done"))}, RunnerOptions{})
	messages := client.Requests()[1].Messages
	if result.Rounds != 2 || len(messages) != 4 || !reflect.DeepEqual(messages[2], text.Response.Assistant) || messages[3].Role != llm.RoleUser || !strings.Contains(messages[3].Content, "task_done") {
		t.Fatal("plain text was lost or treated as completion")
	}
	result, _, _, _ = runScript(t, []llm.MockStep{text, text}, RunnerOptions{})
	if result.Status != model.FileFailed || result.StopReason != "invalid_response" || result.Rounds != 2 {
		t.Fatalf("text-only loop did not stop: %+v", result)
	}
}

func TestRunnerInvalidMetadataDoesNotExecuteAnyCall(t *testing.T) {
	for _, name := range []string{"wrong role", "empty ID", "blank ID", "duplicate ID", "tool result ID"} {
		t.Run(name, func(t *testing.T) {
			step := scriptStep(commentCall("comment"), doneCall("done"))
			switch name {
			case "wrong role":
				step.Response.Assistant.Role = llm.RoleUser
			case "empty ID":
				step.Response.Assistant.ToolCalls[1].ID = ""
			case "blank ID":
				step.Response.Assistant.ToolCalls[1].ID = " \t"
			case "duplicate ID":
				step.Response.Assistant.ToolCalls[1].ID = "comment"
			case "tool result ID":
				step.Response.Assistant.ToolCallID = "unexpected"
			}
			result, usage, client, collector := runScript(t, []llm.MockStep{step, scriptStep(doneCall("valid"))}, RunnerOptions{})
			if result.Rounds != 2 || result.Status != model.FileCompleted || usage.ToolCalls != 1 || len(collector.Findings()) != 0 || len(client.Requests()[1].Messages) != 3 {
				t.Fatalf("invalid response triggered side effects: %+v %+v", result, usage)
			}
		})
	}
}

func TestRunnerRejectsCompletionForAnotherFile(t *testing.T) {
	registry, collector, initial := testRegistry(t, nil)
	finisher, _ := tool.NewTaskFinisher("other.go")
	registry, _ = tool.NewRegistry(tool.Handlers{
		FileRead: func(context.Context, tool.FileReadArgs) (tool.Execution, error) { return tool.Success(struct{}{}), nil },
		CodeComment: func(context.Context, tool.CodeCommentArgs) (tool.Execution, error) {
			return tool.Success(struct{}{}), nil
		},
		TaskDone: finisher.Execute,
	})
	client := llm.NewMockClient([]llm.MockStep{scriptStep(doneCall("wrong")), scriptStep(doneCall("wrong_again"))})
	runner, _ := NewRunner(client, registry, RunnerOptions{})
	result, _, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.Status != model.FileFailed || result.StopReason != "invalid_response" || len(collector.Findings()) != 0 {
		t.Fatalf("wrong file completed: %+v, %v", result, err)
	}
	assertToolMessage(t, client.Requests()[1].Messages[3], "wrong", "invalid_completion")
}

func TestRunnerCancellationStopsRemainingCallsAndKeepsFindings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry, collector, initial := testRegistry(t, func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	})
	client := llm.NewMockClient([]llm.MockStep{scriptStep(commentCall("accepted"), testCall("cancel_read", tool.FileRead, `{"path":"related.go"}`), doneCall("never_run"))})
	runner, _ := NewRunner(client, registry, RunnerOptions{})
	result, usage, err := runner.Run(ctx, "sample.go", initial)
	if !errors.Is(err, context.Canceled) || result.StopReason != "canceled" || result.Status != model.FileFailed || usage.ToolCalls != 2 || len(collector.Findings()) != 1 || len(client.Requests()) != 1 {
		t.Fatalf("cancellation lost findings or continued calls: %+v %+v %v", result, usage, err)
	}
}

func TestRunnerModelFailureAndTimeoutAreFinite(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	cause := errors.New("scripted request failure")
	for _, timeout := range []bool{false, true} {
		var client llm.Client = llm.NewMockClient([]llm.MockStep{{Error: cause}})
		want, reason := cause, "model_error"
		if timeout {
			client = testClient(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
				<-ctx.Done()
				return llm.Response{}, ctx.Err()
			})
			want, reason = context.DeadlineExceeded, "request_timeout"
		}
		runner, _ := NewRunner(client, registry, RunnerOptions{RequestTimeout: 5 * time.Millisecond})
		result, usage, err := runner.Run(context.Background(), "sample.go", initial)
		if !errors.Is(err, want) || result.StopReason != reason || result.Rounds != 1 || usage.LLMRequests != 1 || usage.InputTokens != nil || usage.OutputTokens != nil {
			t.Fatalf("wrong failed request outcome: %+v %+v %v", result, usage, err)
		}
	}
}

func TestRunnerInputAndBudgetPreventRequests(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	client := llm.NewMockClient([]llm.MockStep{scriptStep(doneCall("done"))})
	runner, err := NewRunner(client, registry, RunnerOptions{MaxContextBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, usage, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.StopReason != "context_limit" || usage.LLMRequests != 0 || *usage.InputTokens != 0 || len(client.Requests()) != 0 {
		t.Fatal("over-budget context reached client")
	}
	result, _, err = runner.Run(context.Background(), "sample.go", nil)
	if err == nil || result.StopReason != "invalid_input" || len(client.Requests()) != 0 {
		t.Fatal("invalid initial input reached client")
	}
	for _, options := range []RunnerOptions{{MaxRounds: -1}, {MaxRounds: 21}, {MaxContextBytes: -1}, {RequestTimeout: -1}} {
		if _, err := NewRunner(client, registry, options); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	if _, err := NewRunner(nil, registry, RunnerOptions{}); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := NewRunner(client, nil, RunnerOptions{}); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func TestRunnerSuccessfulToolsResetInvalidRounds(t *testing.T) {
	bad := scriptStep(testCall("bad", "unknown", `{}`))
	steps := []llm.MockStep{bad, scriptStep(commentCall("accepted")), bad, scriptStep(commentCall("duplicate")), scriptStep(doneCall("done"))}
	result, usage, _, collector := runScript(t, steps, RunnerOptions{MaxRounds: 5})
	if result.Status != model.FileCompleted || result.Rounds != 5 || usage.ToolCalls != 5 || len(collector.Findings()) != 1 {
		t.Fatalf("successful or duplicate tool did not reset invalid rounds: %+v %+v", result, usage)
	}
	result, _, _, collector = runScript(t, []llm.MockStep{scriptStep(commentCall("accepted"))}, RunnerOptions{MaxRounds: 1})
	if result.Status != model.FileFailed || result.StopReason != "max_rounds" || result.Rounds != 1 || len(collector.Findings()) != 1 {
		t.Fatalf("round limit lost accepted findings: %+v", result)
	}
}

func TestRunnerBudgetIncludesRawArgumentsAndToolResults(t *testing.T) {
	registry, _, _ := testRegistry(t, nil)
	initial := []llm.Message{{Role: llm.RoleSystem, Content: "abcd"}, {Role: llm.RoleUser, Content: "界"}}
	for _, limit := range []int{6, 7} {
		client := llm.NewMockClient([]llm.MockStep{scriptStep(doneCall("done"))})
		runner, _ := NewRunner(client, registry, RunnerOptions{MaxContextBytes: limit})
		result, _, err := runner.Run(context.Background(), "sample.go", initial)
		if err != nil || (limit == 6 && (result.StopReason != "context_limit" || len(client.Requests()) != 0)) ||
			(limit == 7 && (result.Status != model.FileCompleted || len(client.Requests()) != 1)) {
			t.Fatalf("UTF-8 budget boundary %d: %+v %v", limit, result, err)
		}
	}
	client := llm.NewMockClient([]llm.MockStep{scriptStep(testCall("bad", tool.CodeComment, `{"path":`)), scriptStep(doneCall("never"))})
	runner, _ := NewRunner(client, registry, RunnerOptions{MaxContextBytes: 7})
	result, usage, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.StopReason != "context_limit" || usage.ToolCalls != 1 || len(client.Requests()) != 1 {
		t.Fatalf("tool history bypassed next request budget: %+v %+v %v", result, usage, err)
	}
}

func TestRunnerRequestMutationDoesNotChangeHistoryOrInitialMessages(t *testing.T) {
	registry, _, initial := testRegistry(t, nil)
	want := copyMessages(initial)
	mock := llm.NewMockClient([]llm.MockStep{scriptStep(testCall("read", tool.FileRead, `{"path":"related.go"}`)), scriptStep(doneCall("done"))})
	client := testClient(func(ctx context.Context, request llm.Request) (llm.Response, error) {
		response, err := mock.Complete(ctx, request)
		request.Messages[0].Content = "mutated system"
		request.Tools[0].Parameters[0] = '['
		if len(request.Messages) > 2 {
			request.Messages[2].ToolCalls[0].Arguments[0] = '['
		}
		return response, err
	})
	runner, _ := NewRunner(client, registry, RunnerOptions{})
	for i := 0; i < 2; i++ {
		if i == 1 {
			mock = llm.NewMockClient([]llm.MockStep{scriptStep(doneCall("next_run"))})
		}
		result, _, err := runner.Run(context.Background(), "sample.go", initial)
		if err != nil || result.Status != model.FileCompleted || !reflect.DeepEqual(initial, want) ||
			!reflect.DeepEqual(mock.Requests()[0].Messages, want) {
			t.Fatalf("run %d shared history or caller data: %+v %v", i, result, err)
		}
		if i == 0 && !reflect.DeepEqual(mock.Requests()[1].Messages[2].ToolCalls[0].Arguments, json.RawMessage(`{"path":"related.go"}`)) {
			t.Fatal("request mutation changed retained raw arguments")
		}
	}
}
