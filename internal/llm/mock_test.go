// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMockClientScriptOrderAndExhaustion(t *testing.T) {
	steps := []MockStep{
		{Response: Response{Assistant: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "read_1", Name: "file_read", Arguments: json.RawMessage(`{"path":"related.go"}`)},
		}}}},
		{Response: Response{Assistant: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "comment_1", Name: "code_comment", Arguments: json.RawMessage(`{"path":"sample.go","start_line":5,"end_line":6,"severity":"low","category":"bug","title":"Integer sum may overflow","description":"Large inputs can overflow and produce an incorrect sum.","suggestion":"Check input bounds before addition."}`)},
		}}}},
		{Response: Response{Assistant: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "done_1", Name: "task_done", Arguments: json.RawMessage(`{"summary":"Checked arithmetic and related context."}`)},
		}}}},
	}
	client := NewMockClient(steps)
	ctx := context.Background()
	for i, step := range steps {
		request := Request{Messages: []Message{{Role: RoleUser, Content: step.Response.Assistant.ToolCalls[0].ID}}}
		response, err := client.Complete(ctx, request)
		if err != nil || !reflect.DeepEqual(response, step.Response) {
			t.Fatalf("step %d: response %+v, error %v", i, response, err)
		}
		if got := client.Requests(); len(got) != i+1 || !reflect.DeepEqual(got[i], request) {
			t.Fatalf("step %d request was not recorded: %+v", i, got)
		}
	}
	for i := 0; i < 2; i++ {
		response, err := client.Complete(ctx, Request{})
		if !errors.Is(err, ErrMockScriptExhausted) || !reflect.DeepEqual(response, Response{}) {
			t.Fatalf("exhausted script repeated a response: %+v, %v", response, err)
		}
	}
	if len(client.Requests()) != 5 {
		t.Fatal("exhausted requests were not recorded")
	}
	for _, script := range [][]MockStep{nil, {}} {
		empty := NewMockClient(script)
		if got := empty.Requests(); got == nil || len(got) != 0 {
			t.Fatalf("empty request snapshot: %+v", got)
		}
		if _, err := empty.Complete(ctx, Request{}); !errors.Is(err, ErrMockScriptExhausted) {
			t.Fatalf("empty script error: %v", err)
		}
	}
}

func TestMockClientScriptedErrorConsumesOneStep(t *testing.T) {
	cause := errors.New("scripted model failure")
	want := Response{Assistant: Message{Role: RoleAssistant, Content: "Next scripted response."}}
	client := NewMockClient([]MockStep{
		{Response: want, Error: cause},
		{Response: want},
	})
	response, err := client.Complete(context.Background(), Request{})
	if err != cause || !reflect.DeepEqual(response, Response{}) {
		t.Fatalf("scripted error changed or leaked response: %+v, %v", response, err)
	}
	response, err = client.Complete(context.Background(), Request{})
	if err != nil || !reflect.DeepEqual(response, want) || len(client.Requests()) != 2 {
		t.Fatalf("script did not advance after error: %+v, %v", response, err)
	}
}

func TestMockClientCanceledRequestsDoNotConsumeScript(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			want := Response{Assistant: Message{Role: RoleAssistant, Content: "Unconsumed response."}}
			client := NewMockClient([]MockStep{{Response: want}})
			var ctx context.Context
			var cancel context.CancelFunc
			if cause == context.Canceled {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			} else {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			defer cancel()
			response, err := client.Complete(ctx, Request{})
			if !errors.Is(err, cause) || !reflect.DeepEqual(response, Response{}) || len(client.Requests()) != 0 {
				t.Fatalf("canceled request changed script: %+v, %v", response, err)
			}
			response, err = client.Complete(context.Background(), Request{})
			if err != nil || !reflect.DeepEqual(response, want) || len(client.Requests()) != 1 {
				t.Fatalf("canceled request consumed response: %+v, %v", response, err)
			}
		})
	}
}

func TestMockClientCopiesScriptAndResponse(t *testing.T) {
	input, output := int64(0), int64(7)
	want := Response{
		Assistant: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "bad_1", Name: "code_comment", Arguments: json.RawMessage(`{"path":`)},
		}},
		Usage: &TokenUsage{InputTokens: &input, OutputTokens: &output},
	}
	steps := []MockStep{{Response: want}, {Response: want}}
	client := NewMockClient(steps)
	steps[0].Response.Assistant.ToolCalls[0].Arguments[0] = '['
	steps[0].Response.Assistant.ToolCalls[0].Name = "changed"
	input, output = 99, 99
	steps[0].Response.Usage = nil
	steps[1] = MockStep{Error: errors.New("changed step")}

	for i := 0; i < 2; i++ {
		response, err := client.Complete(context.Background(), Request{})
		if err != nil || response.Assistant.ToolCalls[0].Name != "code_comment" ||
			string(response.Assistant.ToolCalls[0].Arguments) != `{"path":` || response.Usage == nil ||
			*response.Usage.InputTokens != 0 || *response.Usage.OutputTokens != 7 {
			t.Fatalf("script or earlier returned response mutated step %d: %+v, %v", i, response, err)
		}
		response.Assistant.ToolCalls[0].Arguments[0] = '['
		response.Assistant.ToolCalls[0].Name = "changed returned tool"
		*response.Usage.InputTokens = 123
		*response.Usage.OutputTokens = 456
	}
}

func TestMockClientCopiesRequestAndSnapshots(t *testing.T) {
	request := Request{
		Messages: []Message{
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad_1", Name: "code_comment", Arguments: json.RawMessage(`{"path":`)}}},
			{Role: RoleTool, ToolCallID: "bad_1", Content: `{"ok":false}`},
		},
		Tools: []ToolDefinition{{Name: "code_comment", Description: "Submit one finding.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	client := NewMockClient([]MockStep{{}})
	if _, err := client.Complete(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Messages[0].Role = RoleUser
	request.Messages[0].ToolCalls[0].Arguments[0] = '['
	request.Messages[0].ToolCalls[0].ID = "changed"
	request.Messages[1].ToolCallID = "changed"
	request.Tools[0].Name = "changed"
	request.Tools[0].Parameters[0] = '['
	for i := 0; i < 2; i++ {
		got := client.Requests()
		if len(got) != 1 || got[0].Messages[0].Role != RoleAssistant ||
			got[0].Messages[0].ToolCalls[0].ID != "bad_1" || string(got[0].Messages[0].ToolCalls[0].Arguments) != `{"path":` ||
			got[0].Messages[1].ToolCallID != "bad_1" || got[0].Tools[0].Name != "code_comment" ||
			got[0].Messages[1].Content != `{"ok":false}` || got[0].Tools[0].Description != "Submit one finding." ||
			string(got[0].Tools[0].Parameters) != `{"type":"object"}` {
			t.Fatalf("recorded request was mutated: %+v", got)
		}
		got[0].Messages[0].ToolCalls[0].Arguments[0] = '['
		got[0].Messages[0].ToolCalls[0].ID = "changed snapshot"
		got[0].Messages[1].Content = "changed snapshot"
		got[0].Tools[0].Parameters[0] = '['
		got[0].Tools[0].Name = "changed snapshot"
		got[0].Tools[0].Description = "Changed snapshot description."
		got[0].Messages[0].Role = RoleUser
	}
}

func TestMockClientPreservesOptionalUsageAndEmptyCollections(t *testing.T) {
	zero := int64(0)
	for _, want := range []Response{
		{},
		{Usage: &TokenUsage{}},
		{Usage: &TokenUsage{InputTokens: &zero}},
		{Usage: &TokenUsage{OutputTokens: &zero}},
		{Assistant: Message{ToolCalls: []ToolCall{}}},
	} {
		client := NewMockClient([]MockStep{{Response: want}})
		request := Request{Messages: []Message{}, Tools: []ToolDefinition{}}
		got, err := client.Complete(context.Background(), request)
		if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(client.Requests(), []Request{request}) {
			t.Fatalf("optional values or empty collections changed: %+v, %v", got, err)
		}
	}
}
