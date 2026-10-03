// SPDX-License-Identifier: Apache-2.0

// Package model defines the code review result contracts.
package model

// RunStatus describes the outcome of the entire review.
// type RunStatus string的两个优点：
// 1. 类型安全：防止随便传一个普通字符串进来，混淆状态参数。
// 2. 函数参数写 `func SetStatus(st RunStatus)`，就只能传 `RunStatus` 类型，普通字符串不能直接传入，避免传错值。
type RunStatus string

const (
	RunCompleted RunStatus = "completed"
	RunSkipped   RunStatus = "skipped"
	RunPartial   RunStatus = "partial"
	RunFailed    RunStatus = "failed"
)

// FileStatus describes the outcome for one file, independently of the run.
type FileStatus string

const (
	FileCompleted FileStatus = "completed"
	FileSkipped   FileStatus = "skipped"
	FileFailed    FileStatus = "failed"
)

// Severity describes the importance of a finding.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Category identifies the kind of problem reported by a finding.
type Category string

const (
	CategoryBug         Category = "bug"
	CategorySecurity    Category = "security"
	CategoryPerformance Category = "performance"
)

// Finding describes a problem anchored to line numbers in the head commit.
type Finding struct {
	Path        string   `json:"path"`
	StartLine   int      `json:"start_line"`
	EndLine     int      `json:"end_line"`
	Severity    Severity `json:"severity"`
	Category    Category `json:"category"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Suggestion  string   `json:"suggestion"`
}

// FileResult records completion or failure of an individual file review.
type FileResult struct {
	Path       string     `json:"path"`
	Status     FileStatus `json:"status"`
	StopReason string     `json:"stop_reason"`
	Rounds     int        `json:"rounds"`
}

// Usage aggregates requests and tool calls across the run.
// A nil token pointer means usage is unknown and encodes as null.
// A pointer to zero means usage is known to be zero.
type Usage struct {
	LLMRequests  int    `json:"llm_requests"`
	ToolCalls    int    `json:"tool_calls"`
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

// Report holds the complete review result without performing validation.
// Callers must initialize Files, Findings, and Warnings to non-nil slices
// so empty collections encode as [] rather than null.
type Report struct {
	Status     RunStatus    `json:"status"`
	BaseSHA    string       `json:"base_sha"`
	HeadSHA    string       `json:"head_sha"`
	Files      []FileResult `json:"files"`
	Findings   []Finding    `json:"findings"`
	Warnings   []string     `json:"warnings"`
	Usage      Usage        `json:"usage"`
	DurationMS int64        `json:"duration_ms"`
}
