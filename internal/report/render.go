// SPDX-License-Identifier: Apache-2.0

// Package report summarizes review outcomes and renders final reports.
package report

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"
	"unicode"

	"example.com/mini-code-review/internal/agent"
	"example.com/mini-code-review/internal/model"
)

// 作用：Build 函数用于构建代码审查报告，包括文件结果、发现结果、警告信息、使用统计等。
func Build(review agent.ReviewResult, duration time.Duration, fatal bool) model.Report {
	result := model.Report{
		BaseSHA: review.Snapshot.BaseSHA, HeadSHA: review.Snapshot.HeadSHA,
		Files: append([]model.FileResult{}, review.Files...), Findings: append([]model.Finding{}, review.Findings...),
		Warnings: append([]string{}, review.Warnings...), Usage: review.Usage, DurationMS: duration.Milliseconds(),
	}
	// 复制使用统计，避免修改原始数据。
	copyToken := func(value *int64) *int64 {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	result.Usage.InputTokens, result.Usage.OutputTokens = copyToken(review.Usage.InputTokens), copyToken(review.Usage.OutputTokens)
	active, completed := 0, 0
	gap := review.HasCoverageGap
	// 遍历文件结果，统计活动文件数和完成文件数。
	for _, file := range result.Files {
		if file.Status == model.FileSkipped {
			continue
		}
		active++
		if file.Status == model.FileCompleted {
			completed++
		} else {
			gap = true
		}
	}
	// 根据文件状态和发现结果判断代码审查任务的状态。
	switch {
	case fatal:
		result.Status = model.RunFailed
	case !gap && active == 0:
		result.Status = model.RunSkipped
	case !gap && active == completed:
		result.Status = model.RunCompleted
	case completed > 0 || len(result.Findings) > 0:
		result.Status = model.RunPartial
	default:
		result.Status = model.RunFailed
	}
	return result
}

// 作用：Render 函数用于渲染代码审查报告，支持 JSON 和 Markdown 格式。
func Render(result model.Report, format string) ([]byte, error) {
	if result.Files == nil {
		result.Files = []model.FileResult{}
	}
	if result.Findings == nil {
		result.Findings = []model.Finding{}
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	if format == "json" {
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(encoded, '\n'), nil
	}
	if format != "markdown" {
		return nil, fmt.Errorf("unsupported report format")
	}
	// 渲染 Markdown 格式的报告。
	var output strings.Builder
	fmt.Fprintf(&output, "# Mini Code Review\n\nStatus: **%s**\n\nBase: `%s`\n\nHead: `%s`\n\n", result.Status, escape(result.BaseSHA), escape(result.HeadSHA))
	switch result.Status {
	case model.RunCompleted:
		if len(result.Findings) == 0 {
			// 如果没有发现结果，提示用户。
			output.WriteString("No findings reported in this completed review.\n\n")
		}
	case model.RunSkipped:
		output.WriteString("No applicable Go changes.\n\n")
	default:
		output.WriteString("**Review incomplete.** Accepted findings do not establish complete coverage.\n\n")
	}
	// 渲染文件覆盖结果。
	output.WriteString("## File coverage\n\n| File | Status | Stop reason | Rounds |\n| --- | --- | --- | --- |\n")
	for _, file := range result.Files {
		// fmt.Fprintf 用于格式化输出，避免直接拼接字符串。
		fmt.Fprintf(&output, "| %s | %s | %s | %d |\n", escape(file.Path), file.Status, escape(file.StopReason), file.Rounds)
	}
	output.WriteString("\n## Findings\n\n")
	if len(result.Findings) == 0 {
		output.WriteString("No findings recorded.\n\n")
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(&output, "### %s:%d — %s\n\nSeverity: %s; category: %s; lines: %d–%d\n\nCause and impact: %s\n\nSuggestion: %s\n\n", escape(finding.Path), finding.StartLine, escape(finding.Title), finding.Severity, finding.Category, finding.StartLine, finding.EndLine, escape(finding.Description), escape(finding.Suggestion))
	}
	output.WriteString("## Warnings\n\n")
	if len(result.Warnings) == 0 {
		output.WriteString("None.\n\n")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(&output, "- %s\n", escape(warning))
	}
	tokens := func(value *int64) string {
		if value == nil {
			return "unknown"
		}
		return fmt.Sprint(*value)
	}
	fmt.Fprintf(&output, "\n## Usage\n\nModel requests: %d; tool calls: %d\n\nInput tokens: %s; output tokens: %s\n\nDuration: %d ms\n", result.Usage.LLMRequests, result.Usage.ToolCalls, tokens(result.Usage.InputTokens), tokens(result.Usage.OutputTokens), result.DurationMS)
	return []byte(output.String()), nil
}

// 作用：escape 函数用于对 Markdown 格式中的特殊字符进行转义，避免在渲染时出错。
func escape(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = html.EscapeString(value)
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "|", "\\|", "#", "\\#", "\r", " ", "\n", " ").Replace(value)
}
