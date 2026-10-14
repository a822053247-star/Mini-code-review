// SPDX-License-Identifier: Apache-2.0

// Package agent runs file review tasks using model-selected tools.
package agent

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
)

// 作用：定义了代码审查任务的系统提示，用于引导模型生成符合要求的审查结果。
//
//go:embed prompts/review_system.txt
var reviewSystemPrompt string

// 作用：构建代码审查任务的初始消息，包括系统提示和用户数据。
func BuildInitialMessages(set gitdiff.ChangeSet, path, background string) ([]llm.Message, error) {
	if err := gitdiff.ValidateSnapshot(set.Snapshot); err != nil {
		return nil, fmt.Errorf("build review context: %w", err)
	}
	var current *gitdiff.FileChange
	paths := make([]string, 0, len(set.Files))
	for i := range set.Files {
		paths = append(paths, set.Files[i].Path)
		if set.Files[i].Path == path {
			if current != nil {
				return nil, errors.New("current file appears more than once in change set")
			}
			current = &set.Files[i]
		}
	}
	if current == nil || !current.Reviewable || current.SkipReason != "" || len(current.AddedLines) == 0 {
		return nil, errors.New("current file must be a loaded reviewable file with added lines")
	}
	sort.Strings(paths)
	added := make([]int, 0, len(current.AddedLines))
	for line := range current.AddedLines {
		added = append(added, line)
	}
	sort.Ints(added)
	var patch strings.Builder
	for _, line := range current.Lines {
		fmt.Fprintf(&patch, "%s base:%s head:%s %s\n", line.Kind, patchLineNumber(line.OldLine), patchLineNumber(line.NewLine), line.Content)
	}
	data := struct {
		Background   string   `json:"background"`
		BaseSHA      string   `json:"base_sha"`
		HeadSHA      string   `json:"head_sha"`
		ChangedPaths []string `json:"changed_paths"`
		CurrentPath  string   `json:"current_path"`
		Patch        string   `json:"numbered_patch"`
		AddedLines   []int    `json:"added_lines"`
	}{background, set.Snapshot.BaseSHA, set.Snapshot.HeadSHA, paths, path, patch.String(), added}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode review context: %w", err)
	}
	return []llm.Message{
		{Role: llm.RoleSystem, Content: reviewSystemPrompt},
		{Role: llm.RoleUser, Content: string(encoded)},
	}, nil
}

func patchLineNumber(number int) string {
	if number == 0 {
		return "-"
	}
	return fmt.Sprint(number)
}
