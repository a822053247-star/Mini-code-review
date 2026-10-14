// SPDX-License-Identifier: Apache-2.0

package agent

// agent.go 主要去实现 Agent 结构体和相关方法，用于管理和运行代码审查任务。
// 它负责处理 ChangeSet，调用 Runner 来执行每个文件的审查，并收集结果。
import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
	"example.com/mini-code-review/internal/trace"
)

// 作用：标记从start索引开始的所有文件为取消状态，并记录取消原因
type ReviewResult struct {
	Snapshot       gitdiff.Snapshot
	Files          []model.FileResult
	Findings       []model.Finding
	Warnings       []string
	HasCoverageGap bool
	Usage          model.Usage
	FileErrors     map[string]error
}

// Agent assembles loaded file tasks and runs them serially in path order.
type Agent struct {
	client  llm.Client
	options RunnerOptions
	read    tool.HeadFileReader
}

// 作用：创建一个新的Agent实例
func NewAgent(client llm.Client, options RunnerOptions, read tool.HeadFileReader) (*Agent, error) {
	if client == nil {
		return nil, errors.New("agent client must be configured")
	}
	options, err := normalizeRunnerOptions(options)
	if err != nil {
		return nil, err
	}
	return &Agent{client: client, options: options, read: read}, nil
}

// 作用：Run 方法用于运行一个ChangeSet，包括加载文件、创建任务、调用Runner执行任务、收集结果等
func (agent *Agent) Run(ctx context.Context, set gitdiff.ChangeSet, background string) (ReviewResult, error) {
	result := ReviewResult{
		Snapshot: set.Snapshot, Files: []model.FileResult{}, Findings: []model.Finding{},
		Warnings: append([]string{}, set.Warnings...), HasCoverageGap: set.HasCoverageGap,
		Usage: zeroUsage(), FileErrors: make(map[string]error),
	}
	collector := tool.NewCollector()
	set = copyChangeSet(set)
	sort.Slice(set.Files, func(i, j int) bool { return set.Files[i].Path < set.Files[j].Path })
	for _, change := range set.Files {
		file := model.FileResult{Path: change.Path, Status: model.FileFailed, StopReason: "not_started"}
		if !change.Reviewable {
			file.Status, file.StopReason = model.FileSkipped, change.SkipReason
		}
		result.Files = append(result.Files, file)
	}
	if err := ctx.Err(); err != nil {
		markCanceledFiles(&result, set.Files, 0, err)
		return result, fmt.Errorf("review files: %w", err)
	}
	if err := gitdiff.ValidateSnapshot(set.Snapshot); err != nil {
		return result, fmt.Errorf("review change set: %w", err)
	}
	for i := 1; i < len(set.Files); i++ {
		if set.Files[i].Path == set.Files[i-1].Path {
			return result, errors.New("change set file paths must be unique")
		}
	}
	// 作用：运行每个文件的任务，记录结果
	for i, change := range set.Files {
		if parentErr := ctx.Err(); parentErr != nil {
			markCanceledFiles(&result, set.Files, i, parentErr)
			result.Findings = collector.Findings()
			return result, fmt.Errorf("review files: %w", parentErr)
		}
		fileStart := time.Now()
		if err := recordEvent(agent.options.Events, trace.Event{Event: "file_start", File: change.Path}); err != nil {
			markStoppedFiles(&result, set.Files, i, err, "trace_error")
			result.Findings = collector.Findings()
			return result, err
		}
		if !change.Reviewable {
			if err := recordEvent(agent.options.Events, trace.Event{Event: "file_end", File: change.Path, Status: "skipped", StopReason: change.SkipReason, DurationMS: trace.Duration(fileStart)}); err != nil {
				markStoppedFiles(&result, set.Files, i+1, err, "trace_error")
				result.Findings = collector.Findings()
				return result, err
			}
			continue
		}
		file, usage, err := agent.runFile(ctx, set, change, background, collector)
		result.Files[i] = file
		result.Usage.LLMRequests += usage.LLMRequests
		result.Usage.ToolCalls += usage.ToolCalls
		accumulateUsage(&result.Usage, &llm.TokenUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens})
		if err != nil {
			result.FileErrors[change.Path] = err
		}
		if file.Status != model.FileCompleted {
			result.HasCoverageGap = true
		}
		if traceErr := recordEvent(agent.options.Events, trace.Event{Event: "file_end", File: change.Path, Round: file.Rounds, Status: string(file.Status), StopReason: file.StopReason, DurationMS: trace.Duration(fileStart)}); traceErr != nil {
			err = traceErr
		}
		if trace.IsError(err) {
			result.Files[i].Status, result.Files[i].StopReason = model.FileFailed, "trace_error"
			result.FileErrors[change.Path], result.HasCoverageGap = err, true
			markStoppedFiles(&result, set.Files, i+1, err, "trace_error")
			result.Findings = collector.Findings()
			return result, err
		}
		if parentErr := ctx.Err(); parentErr != nil {
			result.Files[i].Status, result.Files[i].StopReason = model.FileFailed, "canceled"
			result.FileErrors[change.Path] = parentErr
			result.HasCoverageGap = true
			markCanceledFiles(&result, set.Files, i+1, parentErr)
			result.Findings = collector.Findings()
			return result, fmt.Errorf("review files: %w", parentErr)
		}
	}
	result.Findings = collector.Findings()
	return result, nil
}

func (agent *Agent) runFile(ctx context.Context, set gitdiff.ChangeSet, change gitdiff.FileChange, background string, collector *tool.Collector) (model.FileResult, model.Usage, error) {
	failure := func(err error) (model.FileResult, model.Usage, error) {
		return model.FileResult{Path: change.Path, Status: model.FileFailed, StopReason: "invalid_input"}, zeroUsage(), fmt.Errorf("assemble file task: %w", err)
	}
	initial, err := BuildInitialMessages(set, change.Path, background)
	if err != nil {
		return failure(err)
	}
	reader, err := tool.NewFileReader(set.Snapshot, agent.read)
	if err != nil {
		return failure(err)
	}
	commenter, err := tool.NewCommenter(change, collector)
	if err != nil {
		return failure(err)
	}
	finisher, err := tool.NewTaskFinisher(change.Path)
	if err != nil {
		return failure(err)
	}
	registry, err := tool.NewRegistry(tool.Handlers{FileRead: reader.Execute, CodeComment: commenter.Execute, TaskDone: finisher.Execute})
	if err != nil {
		return failure(err)
	}
	runner, err := NewRunner(agent.client, registry, agent.options)
	if err != nil {
		return failure(err)
	}
	return runner.Run(ctx, change.Path, initial)
}

func markCanceledFiles(result *ReviewResult, changes []gitdiff.FileChange, start int, cause error) {
	markStoppedFiles(result, changes, start, cause, "canceled")
}

func markStoppedFiles(result *ReviewResult, changes []gitdiff.FileChange, start int, cause error, reason string) {
	for i := start; i < len(changes); i++ {
		if changes[i].Reviewable {
			result.Files[i].Status, result.Files[i].StopReason = model.FileFailed, reason
			result.FileErrors[changes[i].Path] = cause
			result.HasCoverageGap = true
		}
	}
}

// 作用：复制一个ChangeSet，包括所有文件的内容。
// 为什么需要复制？因为ChangeSet是一个引用类型，直接赋值会导致修改原始ChangeSet。
func copyChangeSet(set gitdiff.ChangeSet) gitdiff.ChangeSet {
	set.Files = slices.Clone(set.Files)
	for i := range set.Files {
		set.Files[i].HeadContent = slices.Clone(set.Files[i].HeadContent)
		set.Files[i].Lines = slices.Clone(set.Files[i].Lines)
		added := make(map[int]struct{}, len(set.Files[i].AddedLines))
		for line := range set.Files[i].AddedLines {
			added[line] = struct{}{}
		}
		set.Files[i].AddedLines = added
	}
	set.Warnings = slices.Clone(set.Warnings)
	return set
}
