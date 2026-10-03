// SPDX-License-Identifier: Apache-2.0

// Package llm defines provider-independent model client contracts.
package llm

import (
	"context"
	"encoding/json"
)

// Client performs one model request. Implementations must honor cancellation
// and deadlines in ctx. Model and service configuration belong to the client.
type Client interface {
	Complete(ctx context.Context, request Request) (Response, error)
}

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Request contains conversation history and the tools available to the model.
// These internal types are not a provider's HTTP request format.
type Request struct {
	Messages []Message        `json:"messages"`
	Tools    []ToolDefinition `json:"tools"`
}

// Message represents a text-only conversation turn.
// Assistant messages retain all tool calls. Each tool result uses RoleTool
// and the matching ToolCallID, including results describing a tool error.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall retains arguments as JSON until the selected tool validates them.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDefinition describes a tool and its argument JSON Schema.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Response contains the assistant turn and optional per-request usage.
// A nil Usage means the service did not report token usage.
type Response struct {
	Assistant Message     `json:"assistant"`
	Usage     *TokenUsage `json:"usage"`
}

// TokenUsage records usage for one request, not totals for a review run.
// A nil field means unknown; a pointer to zero means a known zero count.
type TokenUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}
