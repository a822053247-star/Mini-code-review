// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"example.com/mini-code-review/internal/gitdiff"
)

// TaskFinisher 只绑定当前文件路径，不持有 collector 或整体运行状态。
type TaskFinisher struct {
	path string
}

func NewTaskFinisher(path string) (*TaskFinisher, error) {
	if gitdiff.SourcePathReason(path) != "" {
		return nil, errors.New("task file must be an allowed Go source path")
	}
	return &TaskFinisher{path: path}, nil
}

type TaskDoneData struct {
	DoneRequested bool   `json:"done_requested"`
	Path          string `json:"path"`
	Summary       string `json:"summary"`
}

// Execute 产生单文件完成声明，Runner 必须处理同轮所有结果后再决定是否结束。
func (finisher *TaskFinisher) Execute(ctx context.Context, args TaskDoneArgs) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("task_done: %w", err)
	}
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		return Failure("invalid_arguments", "summary must not be blank"), nil
	}
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("task_done: %w", err)
	}
	execution := Success(TaskDoneData{DoneRequested: true, Path: finisher.path, Summary: summary})
	execution.Completion = &Completion{Path: finisher.path, Summary: summary}
	return execution, nil
}
