// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHTTPClient(t *testing.T, server *httptest.Server, timeout time.Duration) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(HTTPConfig{Endpoint: server.URL + "/v1/chat/completions", Model: "fixture-model", APIKey: "fixture-token", RequestTimeout: timeout}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHTTPClientWireFormatPreservesBadArgumentsAndResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request method, endpoint, or headers")
		}
		var payload struct {
			Model     string        `json:"model"`
			Stream    bool          `json:"stream"`
			Thinking  bool          `json:"enable_thinking"`
			MaxTokens int           `json:"max_tokens"`
			Messages  []wireMessage `json:"messages"`
			Tools     []wireTool    `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Model != "fixture-model" || payload.Stream || payload.Thinking || payload.MaxTokens != 4096 || len(payload.Messages) != 4 || len(payload.Tools) != 1 {
			t.Error("incorrect model, generation flags, messages, or tool definitions")
		}
		if payload.Messages[2].ToolCalls[0].Type != "function" || payload.Messages[2].ToolCalls[0].ID != "bad_call" || *payload.Messages[2].ToolCalls[0].Function.Arguments != `{"path":` || payload.Messages[3].ToolCallID != "bad_call" || *payload.Messages[3].Content != `{"ok":false,"error":{"code":"invalid_arguments"}}` {
			t.Error("bad JSON or failed tool association was changed")
		}
		if payload.Tools[0].Type != "function" || payload.Tools[0].Function.Name != "file_read" || !json.Valid(payload.Tools[0].Function.Parameters) {
			t.Error("tool schema was not encoded as an object")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"private reasoning must be ignored","tool_calls":[{"id":"read_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"related.go\"}"}},{"id":"bad_2","type":"function","function":{"name":"code_comment","arguments":"{\"path\":"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":0}}`)
	}))
	defer server.Close()
	client := testHTTPClient(t, server, 0)
	request := Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "Review the current file."}, {Role: RoleUser, Content: "Inspect related context."},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "bad_call", Name: "code_comment", Arguments: json.RawMessage(`{"path":`)}}},
			{Role: RoleTool, ToolCallID: "bad_call", Content: `{"ok":false,"error":{"code":"invalid_arguments"}}`},
		},
		Tools: []ToolDefinition{{Name: "file_read", Description: "Read source.", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	response, err := client.Complete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := Message{Role: RoleAssistant, ToolCalls: []ToolCall{
		{ID: "read_1", Name: "file_read", Arguments: json.RawMessage(`{"path":"related.go"}`)},
		{ID: "bad_2", Name: "code_comment", Arguments: json.RawMessage(`{"path":`)},
	}}
	if !reflect.DeepEqual(response.Assistant, want) || response.Usage == nil || response.Usage.InputTokens == nil || *response.Usage.InputTokens != 0 || response.Usage.OutputTokens != nil {
		t.Fatalf("response lost raw calls or optional usage: %+v", response)
	}
}

func TestHTTPClientErrorsAreSanitizedAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{400, "bad_request"}, {401, "authentication_failed"}, {403, "permission_denied"}, {404, "model_or_endpoint_not_found"}, {429, "rate_limited"}, {503, "service_error"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "fixture-token raw provider error and request content")
			}))
			defer server.Close()
			_, err := testHTTPClient(t, server, 0).Complete(context.Background(), Request{})
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.StatusCode != tc.status || requests.Load() != 1 || strings.Contains(err.Error(), "fixture-token") || strings.Contains(err.Error(), "raw provider") {
				t.Fatalf("unsafe or incorrect error: %v", err)
			}
		})
	}
}

func TestHTTPClientRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	_, err := testHTTPClient(t, origin, 0).Complete(context.Background(), Request{})
	var failure *HTTPError
	if !errors.As(err, &failure) || failure.StatusCode != 307 || redirected.Load() != 0 {
		t.Fatalf("credentials followed a redirect: %v", err)
	}
}

func TestHTTPClientResponseValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"bad JSON", `{"choices":`, "invalid_response"},
		{"missing choices", `{}`, "invalid_response"},
		{"multiple choices", `{"choices":[{},{}]}`, "invalid_response"},
		{"wrong role", `{"choices":[{"message":{"role":"user","content":"x"},"finish_reason":"stop"}]}`, "invalid_response"},
		{"length stop", `{"choices":[{"message":{"role":"assistant"},"finish_reason":"length"}]}`, "response_incomplete"},
		{"unsupported call", `{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"other","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, "invalid_response"},
		{"arguments object", `{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"arguments":{}}}]},"finish_reason":"tool_calls"}]}`, "invalid_response"},
		{"large body", strings.Repeat("x", maxHTTPBodyBytes+1), "response_too_large"},
		{"invalid UTF8", "\xff", "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			response, err := testHTTPClient(t, server, 0).Complete(context.Background(), Request{})
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.Code != tc.code || !reflect.DeepEqual(response, Response{}) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

func TestHTTPClientUsageAndTextResponse(t *testing.T) {
	for _, usage := range []string{"", `,"usage":null`, `,"usage":{}`, `,"usage":{"prompt_tokens":12,"completion_tokens":0}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"Use tools next."},"finish_reason":"stop"}]%s}`, usage)
		}))
		response, err := testHTTPClient(t, server, 0).Complete(context.Background(), Request{})
		server.Close()
		if err != nil || response.Assistant.Content != "Use tools next." {
			t.Fatalf("text response: %+v %v", response, err)
		}
		if usage == "" || strings.Contains(usage, "null") {
			if response.Usage != nil {
				t.Fatal("missing usage became known zero")
			}
		} else if strings.Contains(usage, "prompt_tokens") {
			if response.Usage == nil || *response.Usage.InputTokens != 12 || *response.Usage.OutputTokens != 0 {
				t.Fatal("known usage changed")
			}
		} else if response.Usage == nil || response.Usage.InputTokens != nil || response.Usage.OutputTokens != nil {
			t.Fatal("empty usage fields became known zero")
		}
	}
}

func TestHTTPClientCancellationBeforeAndDuringRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	client := testHTTPClient(t, server, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Complete(ctx, Request{}); !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("pre-canceled request was sent: %v", err)
	}
	if _, err := client.Complete(context.Background(), Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was lost: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("timed-out request was retried")
	}
}

func TestHTTPClientBodyReadHonorsDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	_, err := testHTTPClient(t, server, 20*time.Millisecond).Complete(context.Background(), Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body-read deadline was lost: %v", err)
	}
}

func TestHTTPClientParentCancellationDuringRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := testHTTPClient(t, server, time.Second).Complete(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("active parent cancellation was lost: %v", err)
	}
}

func TestHTTPClientRejectsInvalidSchemasBeforeRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	client := testHTTPClient(t, server, 0)
	for _, schema := range []string{"", "null", "[]", `{"type":`} {
		_, err := client.Complete(context.Background(), Request{Tools: []ToolDefinition{{Name: "file_read", Parameters: json.RawMessage(schema)}}})
		var failure *HTTPError
		if !errors.As(err, &failure) || failure.Code != "invalid_tool_schema" || requests.Load() != 0 {
			t.Fatalf("invalid schema caused a request: %v", err)
		}
	}
}

func TestHTTPClientConfigAndEnvironmentValidation(t *testing.T) {
	t.Setenv("MINI_LLM_ENDPOINT", "https://api.siliconflow.cn/v1/chat/completions")
	t.Setenv("MINI_LLM_MODEL", "deepseek-ai/DeepSeek-V4-Flash")
	t.Setenv("MINI_LLM_API_KEY", "fixture-token")
	valid, err := HTTPConfigFromEnv()
	if err != nil || valid.RequestTimeout != 60*time.Second {
		t.Fatalf("environment config failed: %v", err)
	}
	for _, change := range []func(*HTTPConfig){
		func(c *HTTPConfig) { c.APIKey = "" }, func(c *HTTPConfig) { c.APIKey = "token\nheader" },
		func(c *HTTPConfig) { c.Model = "" }, func(c *HTTPConfig) { c.Endpoint = "https://user:password@example.com/v1/chat/completions" },
		func(c *HTTPConfig) { c.Endpoint = "http://example.com/v1/chat/completions" }, func(c *HTTPConfig) { c.Endpoint += "?key=fixture-token" },
		func(c *HTTPConfig) { c.Endpoint = "https://api.siliconflow.cn/v1" }, func(c *HTTPConfig) { c.RequestTimeout = 61 * time.Second },
	} {
		config := valid
		change(&config)
		if _, err := NewHTTPClient(config, nil); err == nil || strings.Contains(err.Error(), "fixture-token") || strings.Contains(err.Error(), "password") {
			t.Fatal("invalid config accepted or leaked credentials")
		}
	}
	t.Setenv("MINI_LLM_API_KEY", "")
	if _, err := HTTPConfigFromEnv(); err == nil {
		t.Fatal("missing authentication accepted")
	}
	encoded, err := json.Marshal(valid)
	if err != nil || strings.Contains(string(encoded), "fixture-token") {
		t.Fatal("configuration JSON exposed the token")
	}
}
