// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

var ErrMockScriptExhausted = errors.New("mock response script is exhausted")

// MockStep supplies one response or error. Error takes precedence over Response.
type MockStep struct {
	Response Response
	Error    error
}

// MockClient consumes a fixed script and records requests in order.
// It is intended for serial offline use, not concurrent requests.
type MockClient struct {
	steps    []MockStep
	next     int
	requests []Request
}

var _ Client = (*MockClient)(nil)

// NewMockClient copies the script, including raw arguments and usage pointers.
// Invalid tool arguments are retained for downstream validation tests.
func NewMockClient(steps []MockStep) *MockClient {
	client := &MockClient{steps: slices.Clone(steps)}
	for i := range client.steps {
		client.steps[i].Response = cloneMockResponse(steps[i].Response)
	}
	return client
}

// Complete records each active request, including errors and script exhaustion.
// A canceled context does not record the request or consume a script step.
func (client *MockClient) Complete(ctx context.Context, request Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("mock complete: %w", err)
	}
	recorded := cloneMockRequest(request)
	var response Response
	if client.next < len(client.steps) {
		response = cloneMockResponse(client.steps[client.next].Response)
	}
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("mock complete: %w", err)
	}
	client.requests = append(client.requests, recorded)
	if client.next >= len(client.steps) {
		return Response{}, ErrMockScriptExhausted
	}
	step := client.steps[client.next]
	client.next++
	if step.Error != nil {
		return Response{}, step.Error
	}
	return response, nil
}

// Requests returns an independent snapshot of the recorded requests.
func (client *MockClient) Requests() []Request {
	requests := make([]Request, len(client.requests))
	for i := range client.requests {
		requests[i] = cloneMockRequest(client.requests[i])
	}
	return requests
}

func cloneMockMessage(message Message) Message {
	message.ToolCalls = slices.Clone(message.ToolCalls)
	for i := range message.ToolCalls {
		message.ToolCalls[i].Arguments = slices.Clone(message.ToolCalls[i].Arguments)
	}
	return message
}

func cloneMockRequest(request Request) Request {
	request.Messages = slices.Clone(request.Messages)
	for i := range request.Messages {
		request.Messages[i] = cloneMockMessage(request.Messages[i])
	}
	request.Tools = slices.Clone(request.Tools)
	for i := range request.Tools {
		request.Tools[i].Parameters = slices.Clone(request.Tools[i].Parameters)
	}
	return request
}

func cloneMockResponse(response Response) Response {
	response.Assistant = cloneMockMessage(response.Assistant)
	if response.Usage != nil {
		usage := *response.Usage
		if usage.InputTokens != nil {
			value := *usage.InputTokens
			usage.InputTokens = &value
		}
		if usage.OutputTokens != nil {
			value := *usage.OutputTokens
			usage.OutputTokens = &value
		}
		response.Usage = &usage
	}
	return response
}
