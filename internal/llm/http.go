// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxHTTPBodyBytes = 1024 * 1024

// HTTPConfig configures the single SiliconFlow chat-completions protocol.
// Endpoint 表示 SiliconFlow 模型的 API 端点 URL，例如 https://api.siliconflow.cn/v1/chat/completions
type HTTPConfig struct {
	Endpoint       string
	Model          string
	APIKey         string `json:"-"`
	RequestTimeout time.Duration
}

func HTTPConfigFromEnv() (HTTPConfig, error) {
	return validateHTTPConfig(HTTPConfig{
		Endpoint: os.Getenv("MINI_LLM_ENDPOINT"), Model: os.Getenv("MINI_LLM_MODEL"), APIKey: os.Getenv("MINI_LLM_API_KEY"),
	})
}

func validateHTTPConfig(config HTTPConfig) (HTTPConfig, error) {
	config.Endpoint, config.Model, config.APIKey = strings.TrimSpace(config.Endpoint), strings.TrimSpace(config.Model), strings.TrimSpace(config.APIKey)
	if config.APIKey == "" || strings.ContainsAny(config.APIKey, "\r\n\t ") {
		return HTTPConfig{}, errors.New("MINI_LLM_API_KEY must contain a non-blank bearer token")
	}
	if config.Model == "" || strings.ContainsAny(config.Model, "\r\n") {
		return HTTPConfig{}, errors.New("MINI_LLM_MODEL must be configured")
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasSuffix(endpoint.Path, "/chat/completions") {
		return HTTPConfig{}, errors.New("MINI_LLM_ENDPOINT must be a full chat-completions URL without credentials, query, or fragment")
	}
	loopback := endpoint.Hostname() == "localhost"
	if ip := net.ParseIP(endpoint.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
		return HTTPConfig{}, errors.New("model endpoint requires HTTPS; HTTP is allowed only for loopback tests")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 60 * time.Second
	}
	if config.RequestTimeout < 0 || config.RequestTimeout > 60*time.Second {
		return HTTPConfig{}, errors.New("model request timeout must be positive and no more than 60 seconds")
	}
	return config, nil
}

// HTTPError exposes stable diagnostics, never response bodies or auth headers.
// Unwrap preserves transport cancellation and deadline causes.
type HTTPError struct {
	Code       string
	StatusCode int
	cause      error
}

func (err *HTTPError) Error() string {
	message := "model request failed: " + err.Code
	if err.StatusCode != 0 {
		message += " (HTTP " + strconv.Itoa(err.StatusCode) + ")"
	}
	return message
}

func (err *HTTPError) Unwrap() error { return err.cause }

type HTTPClient struct {
	config HTTPConfig
	client http.Client
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient makes no requests. A provided client supplies transport settings;
// redirects are always rejected to keep credentials at the configured endpoint.
func NewHTTPClient(config HTTPConfig, client *http.Client) (*HTTPClient, error) {
	config, err := validateHTTPConfig(config)
	if err != nil {
		return nil, err
	}
	var transport http.Client
	if client != nil {
		transport = *client
	}
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPClient{config: config, client: transport}, nil
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   *string         `json:"arguments,omitempty"`
}

type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

// 作用：Complete 方法用于向 SiliconFlow 模型发送请求，并返回模型的响应结果，包括助手消息和工具调用信息。
func (client *HTTPClient) Complete(ctx context.Context, request Request) (Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, client.config.RequestTimeout)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return Response{}, &HTTPError{Code: "request_canceled", cause: err}
	}
	messages := make([]wireMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		content := message.Content
		encoded := wireMessage{Role: message.Role, Content: &content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			// Arguments are a string on the wire, so invalid JSON can still be
			// returned to the model with its failed tool result on the next turn.
			arguments := string(call.Arguments)
			encoded.ToolCalls = append(encoded.ToolCalls, wireCall{ID: call.ID, Type: "function", Function: wireFunction{Name: call.Name, Arguments: &arguments}})
		}
		messages = append(messages, encoded)
	}
	tools := make([]wireTool, 0, len(request.Tools))
	for _, definition := range request.Tools {
		parameters := bytes.TrimSpace(definition.Parameters)
		if len(parameters) == 0 || parameters[0] != '{' || !json.Valid(parameters) {
			return Response{}, &HTTPError{Code: "invalid_tool_schema"}
		}
		tools = append(tools, wireTool{Type: "function", Function: wireFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.Parameters}})
	}
	payload := struct {
		Model          string        `json:"model"`
		Messages       []wireMessage `json:"messages"`
		Tools          []wireTool    `json:"tools,omitempty"`
		Stream         bool          `json:"stream"`
		EnableThinking bool          `json:"enable_thinking"`
		MaxTokens      int           `json:"max_tokens"`
	}{client.config.Model, messages, tools, false, false, 4096}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > maxHTTPBodyBytes {
		return Response{}, &HTTPError{Code: "invalid_request"}
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, &HTTPError{Code: "invalid_request"}
	}
	// 设置 HTTP 请求头，包括 Authorization、Content-Type 和 Accept。
	httpRequest.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := client.client.Do(httpRequest)
	if err != nil {
		if cause := requestCtx.Err(); cause != nil {
			err = cause
		}
		return Response{}, &HTTPError{Code: "transport_error", cause: err}
	}
	defer response.Body.Close()
	if cause := requestCtx.Err(); cause != nil {
		return Response{}, &HTTPError{Code: "request_canceled", cause: cause}
	}
	if response.StatusCode != http.StatusOK {
		code := "http_error"
		switch response.StatusCode {
		case 400:
			code = "bad_request"
		case 401:
			code = "authentication_failed"
		case 403:
			code = "permission_denied"
		case 404:
			code = "model_or_endpoint_not_found"
		case 429:
			code = "rate_limited"
		default:
			if response.StatusCode >= 500 {
				code = "service_error"
			}
		}
		return Response{}, &HTTPError{Code: code, StatusCode: response.StatusCode}
	}
	// 读取模型响应体，限制最大大小为 max1024MB。
	content, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBodyBytes+1))
	if err != nil {
		if cause := requestCtx.Err(); cause != nil {
			err = cause
		}
		return Response{}, &HTTPError{Code: "response_read_failed", cause: err}
	}
	if len(content) > maxHTTPBodyBytes {
		return Response{}, &HTTPError{Code: "response_too_large"}
	}
	if !utf8.Valid(content) {
		return Response{}, &HTTPError{Code: "invalid_response"}
	}
	// 定义了一个结构体用于解析模型的 JSON 响应，包括消息、完成原因和使用的令牌信息。
	var decoded struct {
		Choices []struct {
			Message      wireMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Input  *int64 `json:"prompt_tokens"`
			Output *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(content, &decoded); err != nil || len(decoded.Choices) != 1 {
		return Response{}, &HTTPError{Code: "invalid_response"}
	}
	choice := decoded.Choices[0]
	if choice.FinishReason != "stop" && choice.FinishReason != "tool_calls" {
		return Response{}, &HTTPError{Code: "response_incomplete"}
	}
	if choice.Message.Role != RoleAssistant {
		return Response{}, &HTTPError{Code: "invalid_response"}
	}
	result := Response{Assistant: Message{Role: RoleAssistant}}
	if choice.Message.Content != nil {
		result.Assistant.Content = *choice.Message.Content
	}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" || call.Function.Arguments == nil {
			return Response{}, &HTTPError{Code: "invalid_response"}
		}
		result.Assistant.ToolCalls = append(result.Assistant.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(*call.Function.Arguments)})
	}
	if decoded.Usage != nil {
		result.Usage = &TokenUsage{InputTokens: decoded.Usage.Input, OutputTokens: decoded.Usage.Output}
	}
	if err := requestCtx.Err(); err != nil {
		return Response{}, &HTTPError{Code: "request_canceled", cause: err}
	}
	return result, nil
}
