// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

func TestHTTPAdapterRunnerRecoversBadArguments(t *testing.T) {
	var count atomic.Int32
	requests := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		var calls []llm.ToolCall
		switch count.Add(1) {
		case 1:
			calls = []llm.ToolCall{doneCall("rejected"), testCall("bad", tool.CodeComment, `{"path":`)}
		case 2:
			calls = []llm.ToolCall{testCall("read", tool.FileRead, `{"path":"related.go"}`)}
		case 3:
			calls = []llm.ToolCall{commentCall("accepted"), commentCall("duplicate"), doneCall("renewed")}
		default:
			t.Error("unexpected hidden request")
			w.WriteHeader(500)
			return
		}
		encoded := []map[string]any{}
		for _, call := range calls {
			encoded = append(encoded, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)}})
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": encoded}, "finish_reason": "tool_calls"}}})
	}))
	defer server.Close()
	client, err := llm.NewHTTPClient(llm.HTTPConfig{Endpoint: server.URL + "/v1/chat/completions", Model: "fixture-model", APIKey: "fixture-token"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	registry, collector, initial := testRegistry(t, nil)
	runner, err := NewRunner(client, registry, RunnerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, usage, err := runner.Run(context.Background(), "sample.go", initial)
	if err != nil || result.Status != model.FileCompleted || result.Rounds != 3 || usage.ToolCalls != 6 || len(collector.Findings()) != 1 {
		t.Fatalf("HTTP recovery loop failed: %+v %+v %v", result, usage, err)
	}
	<-requests
	second := (<-requests)["messages"].([]any)
	assistant := second[2].(map[string]any)
	arguments := assistant["tool_calls"].([]any)[1].(map[string]any)["function"].(map[string]any)["arguments"]
	failed := second[4].(map[string]any)
	if arguments != `{"path":` || failed["role"] != "tool" || failed["tool_call_id"] != "bad" {
		t.Fatal("HTTP request did not retain invalid argument string and result association")
	}
	var failure struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(failed["content"].(string)), &failure); err != nil || failure.OK || failure.Error.Code != "invalid_arguments" {
		t.Fatal("bad JSON was not returned as a tool failure")
	}
	third := (<-requests)["messages"].([]any)
	if third[len(third)-1].(map[string]any)["tool_call_id"] != "read" {
		t.Fatal("read result was not sent on the next HTTP request")
	}
}
