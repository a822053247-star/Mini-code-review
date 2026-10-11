// SPDX-License-Identifier: Apache-2.0

// tool 包负责校验参数并分派三个代码审查工具。
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
)

const (
	FileRead    = "file_read"
	CodeComment = "code_comment"
	TaskDone    = "task_done"
)

// FileReadArgs 用 nil 表示未传入行号，显式传入 0 则属于非法参数。
type FileReadArgs struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line,omitempty"`
	EndLine   *int   `json:"end_line,omitempty"`
}

// CodeCommentArgs 复用已有的 Finding 结构，避免重复定义问题字段。
type CodeCommentArgs = model.Finding

type TaskDoneArgs struct {
	Summary string `json:"summary"`
}

type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result 是返回给模型的统一 JSON 结果，不包含工具调用 ID。
type Result struct {
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *ToolError `json:"error,omitempty"`
}

// Completion 向 Runner 提供当前文件的完成声明，是否完成仍由 Runner 判断。
type Completion struct {
	Path    string
	Summary string
}

// Execution 序列化时只输出 Result，Completion 仅供程序内部使用。
type Execution struct {
	Result
	Completion *Completion `json:"-"`
}

func Success(data any) Execution {
	return Execution{Result: Result{OK: true, Data: data}}
}

func Failure(code, message string) Execution {
	return Execution{Result: Result{Error: &ToolError{Code: code, Message: message}}}
}

// Handlers 绑定三个工具的具体能力，闭包保存固定任务、收集器和 Git 读取函数。
// 注册表本身不读取 Git，也不维护消息历史。
type Handlers struct {
	FileRead    func(context.Context, FileReadArgs) (Execution, error)
	CodeComment func(context.Context, CodeCommentArgs) (Execution, error)
	TaskDone    func(context.Context, TaskDoneArgs) (Execution, error)
}

type Registry struct {
	handlers map[string]func(context.Context, json.RawMessage) (Execution, error)
}

// NewRegistry 要求三个工具的处理函数全部配置，不支持动态添加工具。
// 具体工具能力将在后续步骤中实现并传入。
func NewRegistry(handlers Handlers) (*Registry, error) {
	if handlers.FileRead == nil || handlers.CodeComment == nil || handlers.TaskDone == nil {
		return nil, errors.New("all three tool handlers must be configured")
	}
	return &Registry{handlers: map[string]func(context.Context, json.RawMessage) (Execution, error){
		// FileRead 的 bind 把参数定义、参数校验函数和实际处理函数组合起来，生成一个统一入口，放进注册表。
		FileRead: bind(fileReadSpec, func(args *FileReadArgs) error {
			if strings.TrimSpace(args.Path) == "" {
				return errors.New("path must not be blank")
			}
			if (args.StartLine != nil && *args.StartLine < 1) || (args.EndLine != nil && *args.EndLine < 1) {
				return errors.New("line numbers must be positive integers")
			}
			return nil
		}, handlers.FileRead),
		CodeComment: bind(codeCommentSpec, validateCommentArgs, handlers.CodeComment),
		TaskDone: bind(taskDoneSpec, func(args *TaskDoneArgs) error {
			args.Summary = strings.TrimSpace(args.Summary)
			if args.Summary == "" {
				return errors.New("summary must not be blank")
			}
			return nil
		}, handlers.TaskDone),
	}}, nil
}

// Definitions 按固定顺序返回工具定义，每次生成独立的 Schema 字节切片。
// 调用方修改返回值不会影响注册表后续返回的定义。
func (r *Registry) Definitions() []llm.ToolDefinition {
	// 长度为 0：目前没有元素。
	// 容量为 3：预留三个元素的位置。
	definitions := make([]llm.ToolDefinition, 0, 3)
	for _, spec := range []argumentSpec{fileReadSpec, codeCommentSpec, taskDoneSpec} {
		definitions = append(definitions, llm.ToolDefinition{
			Name: spec.name, Description: spec.description, Parameters: json.RawMessage(spec.schema),
		})
	}
	return definitions
}

// Execute 函数返回工具调用的执行结果。
func (r *Registry) Execute(ctx context.Context, call llm.ToolCall) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("execute tool: %w", err)
	}
	// 找到 map 中 call.Name 对应的处理函数，若不存在则返回统一的失败结果。
	handler, found := r.handlers[call.Name]
	if !found {
		return Failure("unknown_tool", "tool name is not registered"), nil
	}
	// 调用处理函数，若上下文取消或超时则返回统一的失败结果。
	execution, err := handler(ctx, call.Arguments)
	if contextErr := ctx.Err(); contextErr != nil {
		return Execution{}, fmt.Errorf("execute %s: %w", call.Name, contextErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Execution{}, fmt.Errorf("execute %s: %w", call.Name, err)
	}
	if err != nil {
		return Failure("tool_failed", "tool execution failed"), nil
	}
	if (execution.OK && (execution.Data == nil || execution.Error != nil)) ||
		(!execution.OK && (execution.Error == nil || execution.Error.Code == "" || execution.Error.Message == "")) {
		return Failure("tool_failed", "tool returned an invalid result"), nil
	}
	if !execution.OK {
		execution.Data = nil
	}
	if !execution.OK || call.Name != TaskDone {
		execution.Completion = nil
	}
	if _, err := json.Marshal(execution); err != nil {
		return Failure("tool_failed", "tool result could not be serialized"), nil
	}
	return execution, nil
}

func bind[T any](spec argumentSpec, validate func(*T) error, handler func(context.Context, T) (Execution, error)) func(context.Context, json.RawMessage) (Execution, error) {
	return func(ctx context.Context, raw json.RawMessage) (Execution, error) {
		var args T
		if err := decodeArguments(raw, spec, &args); err != nil {
			return Failure("invalid_arguments", err.Error()), nil
		}
		if err := validate(&args); err != nil {
			return Failure("invalid_arguments", err.Error()), nil
		}
		if err := ctx.Err(); err != nil {
			return Execution{}, err
		}
		return handler(ctx, args)
	}
}

// decodeArguments 先严格检查字段名称及必填字段，再解码具体字段类型。
// 逐个读取 token 还能拒绝重复字段，避免 encoding/json 默认将它们合并。
func decodeArguments(raw json.RawMessage, spec argumentSpec, target any) error {
	if !utf8.Valid(raw) {
		return errors.New("arguments must be valid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return errors.New("arguments must be a valid JSON object")
	}
	if token != json.Delim('{') {
		return errors.New("arguments must be a JSON object")
	}
	seen := make(map[string]bool, len(spec.fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("arguments must be a valid JSON object")
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("argument field names must be strings")
		}
		if _, exists := spec.fields[name]; !exists {
			return fmt.Errorf("unknown argument field %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate argument field %q", name)
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("arguments must be a valid JSON object")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("argument field %q must not be null", name)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return errors.New("arguments must be a valid JSON object")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("arguments must contain exactly one JSON object")
	}
	for _, name := range spec.required {
		if !seen[name] {
			return fmt.Errorf("required argument field %q is missing", name)
		}
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("argument fields have invalid types")
	}
	return nil
}

func validateCommentArgs(args *CodeCommentArgs) error {
	if strings.TrimSpace(args.Path) == "" {
		return errors.New("path must not be blank")
	}
	if args.StartLine < 1 || args.EndLine < 1 {
		return errors.New("line numbers must be positive integers")
	}
	switch args.Severity {
	case model.SeverityHigh, model.SeverityMedium, model.SeverityLow:
	default:
		return errors.New("severity must be high, medium, or low")
	}
	switch args.Category {
	case model.CategoryBug, model.CategorySecurity, model.CategoryPerformance:
	default:
		return errors.New("category must be bug, security, or performance")
	}
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{{"title", &args.Title, 120}, {"description", &args.Description, 2000}, {"suggestion", &args.Suggestion, 2000}} {
		*field.value = strings.TrimSpace(*field.value)
		if *field.value == "" || utf8.RuneCountInString(*field.value) > field.limit {
			return fmt.Errorf("%s must contain 1 to %d characters after trimming", field.name, field.limit)
		}
	}
	return nil
}

type argumentSpec struct {
	name, description, schema string
	fields                    map[string]struct{}
	required                  []string
}

var fileReadSpec = argumentSpec{
	name:        FileRead,
	description: "Read numbered Go source from the fixed head commit, including unchanged related files. Reading a file does not authorize comments on it. Output is bounded and may be truncated.",
	fields:      map[string]struct{}{"path": {}, "start_line": {}, "end_line": {}},
	required:    []string{"path"},
	schema: `{
		"type":"object",
		"properties":{
			"path":{"type":"string","minLength":1,"description":"Repository-relative Go file path using forward slashes."},
			"start_line":{"type":"integer","minimum":1,"description":"Optional first head line, inclusive. Defaults to 1."},
			"end_line":{"type":"integer","minimum":1,"description":"Optional last head line, inclusive. When omitted, read up to 120 lines from the start."}
		},
		"required":["path"],"additionalProperties":false
	}`,
}

var codeCommentSpec = argumentSpec{
	name:        CodeComment,
	description: "Submit one finding for the current review file. The start line must be an added head line. Describe the trigger and impact, and provide a repair approach. Valid duplicate submissions succeed without adding another finding.",
	fields:      map[string]struct{}{"path": {}, "start_line": {}, "end_line": {}, "severity": {}, "category": {}, "title": {}, "description": {}, "suggestion": {}},
	required:    []string{"path", "start_line", "end_line", "severity", "category", "title", "description", "suggestion"},
	schema: `{
		"type":"object",
		"properties":{
			"path":{"type":"string","minLength":1,"description":"Must exactly match the current review file."},
			"start_line":{"type":"integer","minimum":1,"description":"Head line belonging to the current file's AddedLines."},
			"end_line":{"type":"integer","minimum":1,"description":"Inclusive head end line; must not precede start_line or exceed EOF."},
			"severity":{"type":"string","enum":["high","medium","low"]},
			"category":{"type":"string","enum":["bug","security","performance"]},
			"title":{"type":"string","minLength":1,"maxLength":120,"description":"Short problem title."},
			"description":{"type":"string","minLength":1,"maxLength":2000,"description":"Explain the triggering conditions and their impact."},
			"suggestion":{"type":"string","minLength":1,"maxLength":2000,"description":"Describe a repair approach; a full patch is not required."}
		},
		"required":["path","start_line","end_line","severity","category","title","description","suggestion"],"additionalProperties":false
	}`,
}

var taskDoneSpec = argumentSpec{
	name:        TaskDone,
	description: "Declare that the current file has been checked and summarize the checks. This may be used with zero findings. The runner decides whether the declaration completes the file task.",
	fields:      map[string]struct{}{"summary": {}},
	required:    []string{"summary"},
	schema: `{
		"type":"object",
		"properties":{"summary":{"type":"string","minLength":1,"description":"Non-blank summary of what was checked in the current file."}},
		"required":["summary"],"additionalProperties":false
	}`,
}
