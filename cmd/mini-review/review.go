// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"example.com/mini-code-review/internal/agent"
	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/report"
	"example.com/mini-code-review/internal/tool"
	"example.com/mini-code-review/internal/trace"
)

type clientFactory func(gitdiff.ChangeSet) (llm.Client, error)

// 作用：runCommand 函数用于执行代码审查命令，包括解析选项、打开输出文件和跟踪文件、
// 初始化日志记录器、加载代码变更集、创建模型客户端、执行代码审查、记录事件、关闭文件等。
func runCommand(ctx context.Context, args []string, stdout, diagnostics io.Writer, factory clientFactory) int {
	options, code := parseOptions(args, diagnostics)
	if code >= 0 {
		return code
	}
	output, traceFile, err := openArtifacts(options.Output, options.Trace)
	if err != nil {
		fmt.Fprintln(diagnostics, "Output preflight failed:", err)
		return 1
	}
	if output != nil {
		defer output.Close()
	}
	if traceFile != nil {
		defer traceFile.Close()
	}
	var logger *trace.Logger
	var events trace.Sink
	if traceFile != nil {
		logger, err = trace.New(traceFile)
		if err != nil {
			fmt.Fprintln(diagnostics, "Trace initialization failed.")
			return 1
		}
		events = logger.Record
	}
	start := time.Now()
	review := agent.ReviewResult{Files: []model.FileResult{}, Findings: []model.Finding{}, Warnings: []string{}, FileErrors: map[string]error{}}
	zeroInput, zeroOutput := int64(0), int64(0)
	review.Usage.InputTokens, review.Usage.OutputTokens = &zeroInput, &zeroOutput
	fatal := false
	emit := func(event trace.Event) error {
		if events == nil {
			return nil
		}
		return events(event)
	}
	// 记录开始事件
	if err = emit(trace.Event{Event: "run_start"}); err == nil {
		var snapshot gitdiff.Snapshot
		snapshot, err = gitdiff.Resolve(ctx, options.Repo, options.Base, options.Head)
		review.Snapshot = snapshot
		if err == nil {
			var set gitdiff.ChangeSet
			set, err = gitdiff.Load(ctx, snapshot)
			review = unstartedReview(set, "input_error")
			if err == nil {
				var client llm.Client
				// 创建模型客户端
				if hasReviewable(set) {
					if factory != nil {
						client, err = factory(set)
					} else if options.Mock {
						client, err = reviewMock(set)
					} else {
						var config llm.HTTPConfig
						config, err = llm.HTTPConfigFromEnv()
						if err == nil {
							client, err = llm.NewHTTPClient(config, nil)
						}
					}
				} else {
					client = llm.NewMockClient(nil)
				}
				if err == nil {
					var reviewer *agent.Agent
					reviewer, err = agent.NewAgent(client, agent.RunnerOptions{MaxRounds: options.MaxRounds, Events: events}, nil)
					if err == nil {
						review, err = reviewer.Run(ctx, set, options.Background)
					}
				}
			}
		}
	}
	if err != nil {
		review.HasCoverageGap = true
		fatal = !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
		if trace.IsError(err) {
			fatal = true
			review.Warnings = append(review.Warnings, "Trace recording failed.")
		} else if fatal {
			review.Warnings = append(review.Warnings, "Review input or configuration failed.")
		} else {
			review.Warnings = append(review.Warnings, "Review canceled before full coverage.")
		}
		// HTTP errors are sanitized; local model causes are never printed here.
		fmt.Fprintln(diagnostics, "Review stopped:", err)
	}
	if options.Mock {
		review.Warnings = append(review.Warnings, "Offline mock demonstration: findings are scripted protocol examples, not model conclusions.")
	}
	for _, file := range review.Files {
		if file.Status == model.FileFailed {
			fmt.Fprintf(diagnostics, "File %s stopped: %s\n", file.Path, file.StopReason)
		}
	}
	// 构建报告
	result := report.Build(review, time.Since(start), fatal)
	if err := emit(trace.Event{Event: "run_end", Status: string(result.Status), DurationMS: trace.Duration(start)}); err != nil {
		result.Status = model.RunFailed
		result.Warnings = append(result.Warnings, "Trace recording failed.")
		fmt.Fprintln(diagnostics, "Trace recording failed.")
	}
	if traceFile != nil {
		if err := traceFile.Sync(); err != nil {
			result.Status = model.RunFailed
			result.Warnings = append(result.Warnings, "Trace flush failed.")
			fmt.Fprintln(diagnostics, "Trace flush failed.")
		}
	}
	// 渲染报告
	encoded, err := report.Render(result, options.Format)
	if err != nil {
		fmt.Fprintln(diagnostics, "Report rendering failed.")
		return 1
	}
	destination := stdout
	if output != nil {
		destination = output
	}
	// 写入报告
	written, err := destination.Write(encoded)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err == nil && output != nil {
		err = output.Sync()
	}
	if err != nil {
		if traceErr := emit(trace.Event{Event: "output_error", Status: "failed", Code: "output_error"}); traceErr != nil {
			fmt.Fprintln(diagnostics, "Trace recording failed.")
		}
		fmt.Fprintln(diagnostics, "Report writing failed.")
		return 1
	}
	if traceFile != nil {
		if err := traceFile.Close(); err != nil {
			fmt.Fprintln(diagnostics, "Trace close failed.")
			return 1
		}
	}
	if output != nil {
		if err := output.Close(); err != nil {
			fmt.Fprintln(diagnostics, "Report close failed.")
			return 1
		}
	}
	if result.Status == model.RunCompleted || result.Status == model.RunSkipped {
		return 0
	}
	return 1
}

func hasReviewable(set gitdiff.ChangeSet) bool {
	for _, file := range set.Files {
		if file.Reviewable {
			return true
		}
	}
	return false
}

// 作用：unstartedReview 函数用于创建一个未开始的代码审查结果，包含所有文件的失败状态和跳过状态。
func unstartedReview(set gitdiff.ChangeSet, reason string) agent.ReviewResult {
	input, output := int64(0), int64(0)
	review := agent.ReviewResult{Snapshot: set.Snapshot, Findings: []model.Finding{}, Files: []model.FileResult{}, Warnings: append([]string{}, set.Warnings...), HasCoverageGap: set.HasCoverageGap, Usage: model.Usage{InputTokens: &input, OutputTokens: &output}, FileErrors: map[string]error{}}
	for _, change := range set.Files {
		file := model.FileResult{Path: change.Path, Status: model.FileFailed, StopReason: reason}
		if !change.Reviewable {
			file.Status, file.StopReason = model.FileSkipped, change.SkipReason
		}
		review.Files = append(review.Files, file)
	}
	sort.Slice(review.Files, func(i, j int) bool { return review.Files[i].Path < review.Files[j].Path })
	return review
}

// 作用：reviewMock 函数用于创建一个模拟的模型客户端，用于测试代码审查功能。
func reviewMock(set gitdiff.ChangeSet) (llm.Client, error) {
	files := append([]gitdiff.FileChange{}, set.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	steps := []llm.MockStep{}
	clients := make(map[string]*llm.MockClient)
	add := func(id, name string, args any) error {
		encoded, err := json.Marshal(args)
		if err != nil {
			return err
		}
		steps = append(steps, llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: encoded}}}}})
		return nil
	}
	for i, file := range files {
		if !file.Reviewable {
			continue
		}
		steps = nil
		anchor := 0
		for line := range file.AddedLines {
			if anchor == 0 || line < anchor {
				anchor = line
			}
		}
		if anchor == 0 {
			return nil, errors.New("mock task requires an added line")
		}
		finding := model.Finding{Path: file.Path, StartLine: anchor, EndLine: anchor, Severity: model.SeverityLow, Category: model.CategoryBug, Title: "Mock protocol example", Description: "This scripted finding demonstrates collection and anchoring; it is not a code-review conclusion.", Suggestion: "Inspect the code independently; no actual defect is asserted by this mock."}
		if err := add(fmt.Sprintf("read_%d", i), tool.FileRead, tool.FileReadArgs{Path: file.Path}); err != nil {
			return nil, err
		}
		if err := add(fmt.Sprintf("comment_%d", i), tool.CodeComment, finding); err != nil {
			return nil, err
		}
		if err := add(fmt.Sprintf("done_%d", i), tool.TaskDone, tool.TaskDoneArgs{Summary: "Checked the offline protocol example."}); err != nil {
			return nil, err
		}
		clients[file.Path] = llm.NewMockClient(steps)
	}
	return &reviewMockClient{clients: clients}, nil
}

type reviewMockClient struct{ clients map[string]*llm.MockClient }

// 作用：Complete 方法用于完成代码审查任务，根据当前文件路径选择对应的模拟客户端并调用其Complete方法。
func (client *reviewMockClient) Complete(ctx context.Context, request llm.Request) (llm.Response, error) {
	if len(request.Messages) < 2 {
		return llm.Response{}, errors.New("mock requires an initial file context")
	}
	var data struct {
		Path string `json:"current_path"`
	}
	if err := json.Unmarshal([]byte(request.Messages[1].Content), &data); err != nil {
		return llm.Response{}, errors.New("mock file context is invalid")
	}
	selected := client.clients[data.Path]
	if selected == nil {
		return llm.Response{}, errors.New("mock has no script for current file")
	}
	return selected.Complete(ctx, request)
}
