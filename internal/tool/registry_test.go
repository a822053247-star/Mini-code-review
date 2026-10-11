// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
)

const validComment = `{"path":"sample.go","start_line":4,"end_line":5,"severity":"high","category":"bug","title":"Wrong result","description":"When inputs differ, subtraction returns an incorrect sum.","suggestion":"Add the inputs."}`

// The spy models handler-side state; it is not the step 4 collector.
type handlerSpy struct {
	calls    []string
	reads    []FileReadArgs
	findings []model.Finding
	done     []TaskDoneArgs
}

func (spy *handlerSpy) handlers() Handlers {
	return Handlers{
		FileRead: func(_ context.Context, args FileReadArgs) (Execution, error) {
			spy.calls = append(spy.calls, FileRead)
			spy.reads = append(spy.reads, args)
			return Success(map[string]string{"path": args.Path}), nil
		},
		CodeComment: func(_ context.Context, args CodeCommentArgs) (Execution, error) {
			spy.calls = append(spy.calls, CodeComment)
			spy.findings = append(spy.findings, args)
			return Success(map[string]bool{"accepted": true, "duplicate": false}), nil
		},
		TaskDone: func(_ context.Context, args TaskDoneArgs) (Execution, error) {
			spy.calls = append(spy.calls, TaskDone)
			spy.done = append(spy.done, args)
			execution := Success(map[string]any{"done_requested": true, "path": "sample.go", "summary": args.Summary})
			execution.Completion = &Completion{Path: "sample.go", Summary: args.Summary}
			return execution, nil
		},
	}
}

func registryWithSpy(t *testing.T) (*Registry, *handlerSpy) {
	t.Helper()
	spy := &handlerSpy{}
	registry, err := NewRegistry(spy.handlers())
	if err != nil {
		t.Fatal(err)
	}
	return registry, spy
}

func TestDefinitionsAreStableAndIsolated(t *testing.T) {
	registry, _ := registryWithSpy(t)
	definitions := registry.Definitions()
	wantNames := []string{FileRead, CodeComment, TaskDone}
	wantRequired := [][]string{{"path"}, {"path", "start_line", "end_line", "severity", "category", "title", "description", "suggestion"}, {"summary"}}
	if len(definitions) != 3 {
		t.Fatalf("got %d definitions", len(definitions))
	}
	for i, definition := range definitions {
		var schema struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if definition.Name != wantNames[i] || definition.Description == "" || schema.Type != "object" ||
			schema.AdditionalProperties == nil || *schema.AdditionalProperties || !reflect.DeepEqual(schema.Required, wantRequired[i]) {
			t.Fatalf("unexpected definition: %+v, schema: %+v", definition, schema)
		}
		for _, name := range schema.Required {
			if _, exists := schema.Properties[name]; !exists {
				t.Fatalf("required field %q missing from schema", name)
			}
		}
	}
	if !reflect.DeepEqual(definitions, registry.Definitions()) {
		t.Fatal("definitions are not stable")
	}
	definitions[0].Name = "changed"
	definitions[1].Parameters[0] = '!'
	again := registry.Definitions()
	if again[0].Name != FileRead || !json.Valid(again[1].Parameters) {
		t.Fatal("caller mutation changed registry definitions")
	}
}

func TestNewRegistryRequiresAllHandlers(t *testing.T) {
	for _, name := range []string{FileRead, CodeComment, TaskDone} {
		t.Run(name, func(t *testing.T) {
			handlers := (&handlerSpy{}).handlers()
			switch name {
			case FileRead:
				handlers.FileRead = nil
			case CodeComment:
				handlers.CodeComment = nil
			case TaskDone:
				handlers.TaskDone = nil
			}
			if registry, err := NewRegistry(handlers); err == nil || registry != nil {
				t.Fatalf("accepted missing handler: registry=%v, error=%v", registry, err)
			}
		})
	}
}

func TestExecuteDispatchesTypedArgumentsAndPreservesCall(t *testing.T) {
	registry, spy := registryWithSpy(t)
	for _, tc := range []struct{ name, raw string }{
		{FileRead, `{"path":"space name.go"}`},
		{FileRead, `{"path":"sample.go","start_line":4,"end_line":5}`},
		{FileRead, `{"path":"sample.go","end_line":5}`},
		{CodeComment, validComment},
		{TaskDone, `{"summary":"  Checked arithmetic. \n"}`},
	} {
		call := llm.ToolCall{ID: "original_call", Name: tc.name, Arguments: json.RawMessage(tc.raw)}
		before := llm.ToolCall{ID: call.ID, Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...)}
		execution, err := registry.Execute(context.Background(), call)
		if err != nil || !execution.OK || execution.Error != nil || !reflect.DeepEqual(call, before) {
			t.Fatalf("dispatch %s: %+v, error %v, call %+v", tc.name, execution, err, call)
		}
		encoded, err := json.Marshal(execution)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil || len(envelope) != 2 || envelope["data"] == nil ||
			envelope["ok"] == nil || envelope["error"] != nil || envelope["Completion"] != nil || envelope["tool_call_id"] != nil {
			t.Fatalf("unexpected success JSON: %s, error %v", encoded, err)
		}
		if (execution.Completion != nil) != (tc.name == TaskDone) {
			t.Fatalf("unexpected completion signal: %+v", execution)
		}
	}
	if !reflect.DeepEqual(spy.calls, []string{FileRead, FileRead, FileRead, CodeComment, TaskDone}) ||
		spy.reads[0].Path != "space name.go" || spy.reads[0].StartLine != nil || spy.reads[0].EndLine != nil ||
		*spy.reads[1].StartLine != 4 || *spy.reads[1].EndLine != 5 || spy.reads[2].StartLine != nil ||
		len(spy.findings) != 1 || spy.findings[0].StartLine != 4 || spy.done[0].Summary != "Checked arithmetic." {
		t.Fatalf("unexpected typed dispatch state: %+v", spy)
	}
}

func assertRejectedWithoutSideEffects(t *testing.T, name, raw, wantCode string) {
	t.Helper()
	registry, spy := registryWithSpy(t)
	spy.findings = []model.Finding{{Path: "existing.go", Title: "Keep existing finding"}}
	before := append([]model.Finding(nil), spy.findings...)
	execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: json.RawMessage(raw)})
	if err != nil || execution.OK || execution.Error == nil || execution.Error.Code != wantCode ||
		execution.Error.Message == "" || execution.Data != nil || execution.Completion != nil ||
		len(spy.calls) != 0 || !reflect.DeepEqual(spy.findings, before) {
		t.Fatalf("rejected call %s %q: %+v, error %v, state %+v", name, raw, execution, err, spy)
	}
	encoded, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil || len(envelope) != 2 || string(envelope["ok"]) != "false" || envelope["error"] == nil {
		t.Fatalf("unexpected failure JSON: %s, error %v", encoded, err)
	}
}

func TestExecuteRejectsMalformedArgumentsBeforeDispatch(t *testing.T) {
	for _, raw := range []string{
		"", "{", "[]", "null", "true", "1", `"text"`, `{"path":}`, `{"path":"sample.go",}`,
		`{"path":"sample.go"} {}`, `{"path":"sample.go"} null`, `{"path":"sample.go"} garbage`,
		`{"path":"sample.go","extra":1}`, `{"Path":"sample.go"}`,
		`{"path":"sample.go","PATH":"other.go"}`, `{"path":"sample.go","path":"other.go"}`,
		`{"path":"sample.go","\u0070ath":"other.go"}`, `{"path":"sample.go","start_line":1.5}`,
		`{"path":"sample.go","start_line":999999999999999999999999999}`, "{\"path\":\"\xff.go\"}",
	} {
		t.Run(fmt.Sprintf("input_%q", raw), func(t *testing.T) {
			assertRejectedWithoutSideEffects(t, FileRead, raw, "invalid_arguments")
		})
	}
	for _, name := range []string{"missing_tool", "", "FILE_READ"} {
		assertRejectedWithoutSideEffects(t, name, `{}`, "unknown_tool")
	}
}

func TestExecuteRejectsMissingNullAndWrongTypesForEveryField(t *testing.T) {
	for _, tc := range []struct {
		name, valid string
		spec        argumentSpec
	}{
		{FileRead, `{"path":"sample.go","start_line":4,"end_line":5}`, fileReadSpec},
		{CodeComment, validComment, codeCommentSpec},
		{TaskDone, `{"summary":"Checked."}`, taskDoneSpec},
	} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.valid), &fields); err != nil {
			t.Fatal(err)
		}
		for name := range tc.spec.fields {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				original := fields[name]
				for _, replacement := range []string{"null", "true", "[]", "{}"} {
					fields[name] = json.RawMessage(replacement)
					encoded, err := json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					assertRejectedWithoutSideEffects(t, tc.name, string(encoded), "invalid_arguments")
				}
				fields[name] = original
			})
		}
		for _, name := range tc.spec.required {
			original := fields[name]
			delete(fields, name)
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			assertRejectedWithoutSideEffects(t, tc.name, string(encoded), "invalid_arguments")
			fields[name] = original
		}
	}
}

func TestExecuteValidatesStaticFieldConstraints(t *testing.T) {
	for _, raw := range []string{
		`{"path":" "}`, `{"path":1}`, `{"path":"sample.go","start_line":0}`,
		`{"path":"sample.go","end_line":-1}`, `{"path":"sample.go","start_line":"4"}`,
	} {
		assertRejectedWithoutSideEffects(t, FileRead, raw, "invalid_arguments")
	}
	assertRejectedWithoutSideEffects(t, TaskDone, `{"summary":" \t\n"}`, "invalid_arguments")
	for _, tc := range []struct {
		field string
		value any
	}{
		{"severity", "critical"}, {"severity", "HIGH"}, {"category", "style"}, {"category", "Bug"},
		{"start_line", 0}, {"end_line", -1}, {"title", " \n"}, {"description", ""}, {"suggestion", "\t"},
		{"title", strings.Repeat("中", 121)}, {"description", strings.Repeat("中", 2001)}, {"suggestion", strings.Repeat("a", 2001)},
	} {
		var fields map[string]any
		if err := json.Unmarshal([]byte(validComment), &fields); err != nil {
			t.Fatal(err)
		}
		fields[tc.field] = tc.value
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedWithoutSideEffects(t, CodeComment, string(encoded), "invalid_arguments")
	}
	registry, spy := registryWithSpy(t)
	var finding model.Finding
	if err := json.Unmarshal([]byte(validComment), &finding); err != nil {
		t.Fatal(err)
	}
	finding.Title = " " + strings.Repeat("中", 120) + " "
	finding.Description = strings.Repeat("中", 2000)
	finding.Suggestion = strings.Repeat("a", 2000)
	encoded, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: CodeComment, Arguments: encoded})
	if err != nil || !execution.OK || len(spy.findings) != 1 || spy.findings[0].Title != strings.TrimSpace(finding.Title) {
		t.Fatalf("valid Unicode boundaries rejected: %+v, error %v", execution, err)
	}
}

func TestExecutePropagatesCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			registry, spy := registryWithSpy(t)
			var ctx context.Context
			var cancel context.CancelFunc
			if cause == context.Canceled {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			} else {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			defer cancel()
			execution, err := registry.Execute(ctx, llm.ToolCall{Name: FileRead, Arguments: json.RawMessage(`{"path":"sample.go"}`)})
			if !errors.Is(err, cause) || !reflect.DeepEqual(execution, Execution{}) || len(spy.calls) != 0 {
				t.Fatalf("cancellation: %+v, error %v, calls %v", execution, err, spy.calls)
			}
			// Simulate a canceled or timed-out Git child while the parent is active.
			handlers := spy.handlers()
			handlers.FileRead = func(context.Context, FileReadArgs) (Execution, error) {
				return Success("discard"), fmt.Errorf("read head file: %w", cause)
			}
			registry, err = NewRegistry(handlers)
			if err != nil {
				t.Fatal(err)
			}
			execution, err = registry.Execute(context.Background(), llm.ToolCall{Name: FileRead, Arguments: json.RawMessage(`{"path":"sample.go"}`)})
			if !errors.Is(err, cause) || !reflect.DeepEqual(execution, Execution{}) {
				t.Fatalf("wrapped child cancellation: %+v, error %v", execution, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlers := (&handlerSpy{}).handlers()
	handlers.TaskDone = func(context.Context, TaskDoneArgs) (Execution, error) {
		cancel()
		execution := Success(map[string]bool{"done_requested": true})
		execution.Completion = &Completion{Path: "sample.go", Summary: "Checked."}
		return execution, nil
	}
	registry, err := NewRegistry(handlers)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := registry.Execute(ctx, llm.ToolCall{Name: TaskDone, Arguments: json.RawMessage(`{"summary":"Checked."}`)})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(execution, Execution{}) {
		t.Fatalf("completion survived cancellation: %+v, error %v", execution, err)
	}
}

func TestExecuteNormalizesHandlerFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reply    Execution
		err      error
		wantCode string
	}{
		{"ordinary failure", Failure("read_failed", "head content could not be read"), nil, "read_failed"},
		{"unexpected error", Success("discard"), errors.New("private backend diagnostic"), "tool_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handlers := (&handlerSpy{}).handlers()
			handlers.TaskDone = func(context.Context, TaskDoneArgs) (Execution, error) {
				reply := tc.reply
				reply.Completion = &Completion{Path: "sample.go", Summary: "discard"}
				return reply, tc.err
			}
			registry, err := NewRegistry(handlers)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: TaskDone, Arguments: json.RawMessage(`{"summary":"Checked."}`)})
			if err != nil || execution.OK || execution.Error == nil || execution.Error.Code != tc.wantCode || execution.Completion != nil || execution.Data != nil {
				t.Fatalf("handler failure: %+v, error %v", execution, err)
			}
		})
	}
}

func TestExecuteReturnsSerializableConsistentResults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply Execution
	}{
		{"empty result", Execution{}},
		{"missing success data", Success(nil)},
		{"conflicting branches", Execution{Result: Result{OK: true, Data: "data", Error: &ToolError{Code: "error", Message: "conflict"}}}},
		{"unsupported data", Success(make(chan int))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handlers := (&handlerSpy{}).handlers()
			handlers.FileRead = func(context.Context, FileReadArgs) (Execution, error) { return tc.reply, nil }
			registry, err := NewRegistry(handlers)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: FileRead, Arguments: json.RawMessage(`{"path":"sample.go"}`)})
			if err != nil || execution.OK || execution.Error == nil || execution.Error.Code != "tool_failed" || execution.Completion != nil {
				t.Fatalf("invalid result survived: %+v, error %v", execution, err)
			}
			if _, err := json.Marshal(execution); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A read handler cannot declare file completion.
	handlers := (&handlerSpy{}).handlers()
	handlers.FileRead = func(context.Context, FileReadArgs) (Execution, error) {
		execution := Success("content")
		execution.Completion = &Completion{Path: "sample.go", Summary: "discard"}
		return execution, nil
	}
	registry, err := NewRegistry(handlers)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: FileRead, Arguments: json.RawMessage(`{"path":"sample.go"}`)})
	if err != nil || !execution.OK || execution.Completion != nil {
		t.Fatalf("read handler produced completion: %+v, error %v", execution, err)
	}
}
