// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

type RunnerOptions struct {
	MaxRounds       int
	MaxContextBytes int
	RequestTimeout  time.Duration
}

type Runner struct {
	client   llm.Client
	registry *tool.Registry
	options  RunnerOptions
}

// 作用：创建一个代码审查任务的运行器，用于执行代码审查任务。
func NewRunner(client llm.Client, registry *tool.Registry, options RunnerOptions) (*Runner, error) {
	if client == nil || registry == nil {
		return nil, errors.New("runner client and registry must be configured")
	}
	options, err := normalizeRunnerOptions(options)
	if err != nil {
		return nil, err
	}
	return &Runner{client: client, registry: registry, options: options}, nil
}

func normalizeRunnerOptions(options RunnerOptions) (RunnerOptions, error) {
	if options.MaxRounds == 0 {
		options.MaxRounds = 8
	}
	if options.MaxContextBytes == 0 {
		options.MaxContextBytes = 64 * 1024
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 60 * time.Second
	}
	if options.MaxRounds < 1 || options.MaxRounds > 20 || options.MaxContextBytes < 1 || options.MaxContextBytes > 64*1024 || options.RequestTimeout < 0 || options.RequestTimeout > 60*time.Second {
		return RunnerOptions{}, errors.New("runner limits must be positive; maximums are 20 rounds, 64 KiB context, and 60 seconds per request")
	}
	return options, nil
}

func zeroUsage() model.Usage {
	input, output := int64(0), int64(0)
	return model.Usage{InputTokens: &input, OutputTokens: &output}
}

// 作用：执行代码审查任务，返回任务结果和使用统计。
func (runner *Runner) Run(ctx context.Context, path string, initial []llm.Message) (model.FileResult, model.Usage, error) {
	// 阶段一: 验证输入参数和初始消息，确保当前文件是可审查的，并且初始消息符合要求。
	result := model.FileResult{Path: path, Status: model.FileFailed}
	usage := zeroUsage()
	stop := func(reason string, err error) (model.FileResult, model.Usage, error) {
		result.StopReason = reason
		return result, usage, err
	}
	if err := ctx.Err(); err != nil {
		return stop("canceled", fmt.Errorf("run file: %w", err))
	}
	if gitdiff.SourcePathReason(path) != "" || len(initial) != 2 || initial[0].Role != llm.RoleSystem || initial[1].Role != llm.RoleUser {
		return stop("invalid_input", errors.New("runner requires an allowed source path and initial system/user messages"))
	}
	for _, message := range initial {
		if len(message.ToolCalls) != 0 || message.ToolCallID != "" {
			return stop("invalid_input", errors.New("initial messages must not contain tool calls or results"))
		}
	}
	history := copyMessages(initial)
	invalidRounds := 0
	// 阶段二: 循环执行代码审查任务，直到任务完成或超过最大轮数。
	for {
		if err := ctx.Err(); err != nil {
			return stop("canceled", fmt.Errorf("run file: %w", err))
		}
		if result.Rounds >= runner.options.MaxRounds {
			return stop("max_rounds", nil)
		}
		if !withinContextBudget(history, runner.options.MaxContextBytes) {
			return stop("context_limit", nil)
		}
		requestCtx, cancel := context.WithTimeout(ctx, runner.options.RequestTimeout)
		result.Rounds++
		usage.LLMRequests++
		response, err := runner.client.Complete(requestCtx, llm.Request{Messages: copyMessages(history), Tools: runner.registry.Definitions()})
		requestErr := requestCtx.Err()
		cancel()
		accumulateUsage(&usage, response.Usage)
		if err != nil || requestErr != nil {
			if parentErr := ctx.Err(); parentErr != nil {
				return stop("canceled", fmt.Errorf("request model: %w", parentErr))
			}
			if requestErr != nil {
				err = requestErr
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return stop("request_timeout", fmt.Errorf("request model: %w", err))
			}
			return stop("model_error", fmt.Errorf("request model: %w", err))
		}
		if err := ctx.Err(); err != nil {
			return stop("canceled", fmt.Errorf("run file: %w", err))
		}
		successful := false
		if !validAssistant(response.Assistant) {
			history = append(history, llm.Message{Role: llm.RoleUser, Content: "Return an assistant response with non-blank, unique tool call IDs so each result can be associated with its call."})
		} else {
			// 阶段三: 处理模型生成的Assistant消息，包括工具调用和完成任务的声明。
			assistant := copyMessages([]llm.Message{response.Assistant})[0]
			history = append(history, assistant)
			// 如果没有工具调用，提示模型继续使用工具审查当前文件，并声明完成任务。
			if len(assistant.ToolCalls) == 0 {
				history = append(history, llm.Message{Role: llm.RoleUser, Content: "Continue the current file review using the tools. Declare completion with task_done; plain text does not complete the task."})
			}
			roundFailed, done := false, false
			for _, call := range assistant.ToolCalls {
				if err := ctx.Err(); err != nil {
					return stop("canceled", fmt.Errorf("execute tools: %w", err))
				}
				usage.ToolCalls++
				// 调用 runner.registry.Execute 执行工具调用。
				execution, err := runner.registry.Execute(ctx, call)
				if err != nil {
					if parentErr := ctx.Err(); parentErr != nil {
						return stop("canceled", fmt.Errorf("execute tool: %w", parentErr))
					}
					if errors.Is(err, context.DeadlineExceeded) {
						return stop("tool_timeout", fmt.Errorf("execute tool: %w", err))
					}
					return stop("tool_error", fmt.Errorf("execute tool: %w", err))
				}
				if call.Name == tool.TaskDone && execution.OK && (execution.Completion == nil || execution.Completion.Path != path) {
					execution = tool.Failure("invalid_completion", "completion must declare the current file")
				}
				encoded, err := json.Marshal(execution.Result)
				if err != nil {
					return stop("tool_error", fmt.Errorf("encode tool result: %w", err))
				}
				history = append(history, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: string(encoded)})
				if execution.OK {
					successful = true
					if execution.Completion != nil {
						done = true
					}
				} else {
					roundFailed = true
				}
			}
			if err := ctx.Err(); err != nil {
				return stop("canceled", fmt.Errorf("finish tool round: %w", err))
			}
			if done && !roundFailed {
				result.Status = model.FileCompleted
				return stop("task_done", nil)
			}
		}
		if successful {
			invalidRounds = 0
		} else {
			invalidRounds++
		}
		if invalidRounds >= 2 {
			if err := ctx.Err(); err != nil {
				return stop("canceled", fmt.Errorf("finish invalid round: %w", err))
			}
			return stop("invalid_response", nil)
		}
	}
}

func validAssistant(message llm.Message) bool {
	if message.Role != llm.RoleAssistant || message.ToolCallID != "" {
		return false
	}
	seen := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || seen[call.ID] {
			return false
		}
		seen[call.ID] = true
	}
	return true
}

func copyMessages(messages []llm.Message) []llm.Message {
	messages = slices.Clone(messages)
	for i := range messages {
		messages[i].ToolCalls = slices.Clone(messages[i].ToolCalls)
		for j := range messages[i].ToolCalls {
			messages[i].ToolCalls[j].Arguments = slices.Clone(messages[i].ToolCalls[j].Arguments)
		}
	}
	return messages
}

func withinContextBudget(messages []llm.Message, limit int) bool {
	remaining := limit
	for _, message := range messages {
		remaining -= len(message.Content)
		if remaining < 0 {
			return false
		}
		for _, call := range message.ToolCalls {
			remaining -= len(call.Arguments)
			if remaining < 0 {
				return false
			}
		}
	}
	return true
}

// 作用：累加代码审查任务的使用统计，包括输入令牌数、输出令牌数和工具调用次数。
func accumulateUsage(total *model.Usage, usage *llm.TokenUsage) {
	if usage == nil {
		total.InputTokens, total.OutputTokens = nil, nil
		return
	}
	addTokens := func(sum **int64, value *int64) {
		if *sum == nil || value == nil || *value < 0 || **sum > math.MaxInt64-*value {
			*sum = nil
			return
		}
		**sum += *value
	}
	addTokens(&total.InputTokens, usage.InputTokens)
	addTokens(&total.OutputTokens, usage.OutputTokens)
}
