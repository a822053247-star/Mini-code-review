// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"errors"
	"fmt"

	"example.com/mini-code-review/internal/gitdiff"
)

// Commenter 绑定来自 Load 的当前文件，独立保存行数与新增锚点。
// 文件与 Snapshot 的来源一致性由装配方保证，不接受模型重新指定任务。
type Commenter struct {
	path       string
	totalLines int
	addedLines map[int]struct{}
	collector  *Collector
}

// NewCommenter 创建一个 Commenter 实例，绑定当前文件和评论收集器。
func NewCommenter(change gitdiff.FileChange, collector *Collector) (*Commenter, error) {
	if collector == nil {
		return nil, errors.New("comment collector must be configured")
	}
	if !change.Reviewable || change.SkipReason != "" || change.Type == gitdiff.ChangeDeleted ||
		gitdiff.SourcePathReason(change.Path) != "" || gitdiff.SourceContentReason(change.HeadContent) != "" ||
		len(change.HeadContent) > gitdiff.MaxHeadFileBytes {
		return nil, errors.New("current file must be a reviewable Go source file")
	}
	total := len(SplitSourceLines(change.HeadContent))
	if len(change.AddedLines) == 0 {
		return nil, errors.New("current file must have added head lines")
	}
	added := make(map[int]struct{}, len(change.AddedLines))
	for line := range change.AddedLines {
		if line < 1 || line > total {
			return nil, errors.New("added head line is outside the current file")
		}
		added[line] = struct{}{}
	}
	return &Commenter{path: change.Path, totalLines: total, addedLines: added, collector: collector}, nil
}

type CommentData struct {
	Accepted  bool `json:"accepted"`
	Duplicate bool `json:"duplicate"`
}

// Execute 只校验字段和 head 锚点，不证明问题的触发条件或影响真实。
func (commenter *Commenter) Execute(ctx context.Context, args CodeCommentArgs) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("code_comment: %w", err)
	}
	// 直接调用具体工具时也执行静态校验，复用注册表规则。
	if err := validateCommentArgs(&args); err != nil {
		return Failure("invalid_arguments", err.Error()), nil
	}
	if args.Path != commenter.path {
		return Failure("wrong_file", "path must exactly match the current review file"), nil
	}
	if args.StartLine > args.EndLine || args.EndLine > commenter.totalLines {
		return Failure("invalid_range", "comment range must be within the head file"), nil
	}
	if _, exists := commenter.addedLines[args.StartLine]; !exists {
		return Failure("invalid_anchor", "start_line must be an added line in the current file"), nil
	}
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("code_comment: %w", err)
	}
	duplicate := commenter.collector.add(args)
	return Success(CommentData{Accepted: true, Duplicate: duplicate}), nil
}
