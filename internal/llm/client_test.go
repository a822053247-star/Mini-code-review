// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRequestPreservesToolCallsAndResults(t *testing.T) {
	request := Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "Review the changed code."},
			{Role: RoleUser, Content: "Check service.go."},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "file_read", Arguments: json.RawMessage(`{"path":"service.go"}`)},
				{ID: "call_2", Name: "file_read", Arguments: json.RawMessage(`{"path":"helper.go"}`)},
			}},
			{Role: RoleTool, ToolCallID: "call_1", Content: `{"ok":true,"content":"1: package service"}`},
			{Role: RoleTool, ToolCallID: "call_2", Content: `{"ok":false,"error":"file not found"}`},
		},
		Tools: []ToolDefinition{{
			Name: "file_read", Description: "Read a file from the head commit.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Request
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatalf("conversation contract changed after JSON round trip: %s", encoded)
	}
	// RawMessage must encode as a JSON object, not an escaped string.
	var wire struct {
		Messages []struct {
			ToolCalls []struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Messages[2].ToolCalls[0].Arguments["path"] != "service.go" || wire.Tools[0].Parameters["type"] != "object" {
		t.Fatalf("tool arguments or schema were not preserved: %s", encoded)
	}
}

func TestResponseOptionalUsage(t *testing.T) {
	zero := int64(0)
	for _, tc := range []struct {
		name     string
		usage    *TokenUsage
		expected string
	}{
		{name: "not reported", expected: `null`},
		{name: "unknown fields", usage: &TokenUsage{}, expected: `{"input_tokens":null,"output_tokens":null}`},
		{name: "known zero and unknown", usage: &TokenUsage{InputTokens: &zero}, expected: `{"input_tokens":0,"output_tokens":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(Response{Assistant: Message{Role: RoleAssistant}, Usage: tc.usage})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["usage"]) != tc.expected {
				t.Fatalf("usage = %s, want %s", fields["usage"], tc.expected)
			}
		})
	}
}
