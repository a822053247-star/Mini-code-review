// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
)

func commentFixture(path string) gitdiff.FileChange {
	return gitdiff.FileChange{
		Path: path, Type: gitdiff.ChangeModified, Reviewable: true,
		HeadContent: []byte("package sample\n\nfunc Sum(a, b int) int {\n\t// Add inputs.\n\treturn a + b\n}\n"),
		ParsedPatch: gitdiff.ParsedPatch{AddedLines: map[int]struct{}{4: {}, 5: {}}},
	}
}

func commentFinding(t *testing.T) model.Finding {
	t.Helper()
	var finding model.Finding
	if err := json.Unmarshal([]byte(validComment), &finding); err != nil {
		t.Fatal(err)
	}
	return finding
}

// 绑定三个真实工具；内存读取避免单元测试依赖 Git fixture。
func commentRegistry(t *testing.T, path string, collector *Collector) *Registry {
	t.Helper()
	change := commentFixture(path)
	commenter, err := NewCommenter(change, collector)
	if err != nil {
		t.Fatal(err)
	}
	finisher, err := NewTaskFinisher(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
		return change.HeadContent, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(Handlers{FileRead: reader.Execute, CodeComment: commenter.Execute, TaskDone: finisher.Execute})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func executeComment(t *testing.T, registry *Registry, finding model.Finding) Execution {
	t.Helper()
	arguments, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: CodeComment, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func TestCodeCommentAcceptsAddedStartAndContextEnd(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	finding := commentFinding(t)
	finding.EndLine = 6 // 第 6 行是上下文，只有起点必须属于新增集合。
	finding.Title = "  Wrong result  "
	finding.Description = " " + finding.Description + " "
	finding.Suggestion = "\n" + finding.Suggestion + "\n"
	execution := executeComment(t, registry, finding)
	if execution.Data != (CommentData{Accepted: true}) || !execution.OK || execution.Completion != nil {
		t.Fatalf("legal finding rejected: %+v", execution)
	}
	want := finding
	want.Title = strings.TrimSpace(want.Title)
	want.Description = strings.TrimSpace(want.Description)
	want.Suggestion = strings.TrimSpace(want.Suggestion)
	if !reflect.DeepEqual(collector.Findings(), []model.Finding{want}) {
		t.Fatalf("unexpected collected finding: %+v", collector.Findings())
	}
}

func TestCodeCommentRejectsInvalidFieldsWithoutMutation(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	valid := commentFinding(t)
	if !executeComment(t, registry, valid).OK {
		t.Fatal("fixture finding rejected")
	}
	before := collector.Findings()
	for _, tc := range []struct {
		name, code string
		change     func(*model.Finding)
	}{
		{"other file", "wrong_file", func(f *model.Finding) { f.Path = "related.go" }},
		{"normalized path", "wrong_file", func(f *model.Finding) { f.Path = "./sample.go" }},
		{"case changed path", "wrong_file", func(f *model.Finding) { f.Path = "Sample.go" }},
		{"zero start", "invalid_arguments", func(f *model.Finding) { f.StartLine = 0 }},
		{"negative end", "invalid_arguments", func(f *model.Finding) { f.EndLine = -1 }},
		{"reversed", "invalid_range", func(f *model.Finding) { f.EndLine = 3 }},
		{"beyond EOF", "invalid_range", func(f *model.Finding) { f.EndLine = 7 }},
		{"trailing newline is not a line", "invalid_range", func(f *model.Finding) { f.StartLine = 7; f.EndLine = 7 }},
		{"context start", "invalid_anchor", func(f *model.Finding) { f.StartLine = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finding := valid
			tc.change(&finding)
			execution := executeComment(t, registry, finding)
			if execution.OK || execution.Error == nil || execution.Error.Code != tc.code || execution.Completion != nil || !reflect.DeepEqual(collector.Findings(), before) {
				t.Fatalf("invalid finding mutated collector: %+v, findings %+v", execution, collector.Findings())
			}
		})
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(validComment), &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range codeCommentSpec.required {
		original := fields[name]
		for _, mode := range []string{"missing", "null", "blank", "invalid"} {
			switch mode {
			case "missing":
				delete(fields, name)
			case "null":
				fields[name] = nil
			case "blank":
				fields[name] = " \t\n"
			case "invalid":
				fields[name] = "unsupported"
			}
			arguments, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			// 普通非空文本允许，无法机械判断其语义真实性。
			if mode == "invalid" && (name == "title" || name == "description" || name == "suggestion") {
				continue
			}
			execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: CodeComment, Arguments: arguments})
			if err != nil || execution.OK || execution.Error == nil || !reflect.DeepEqual(collector.Findings(), before) {
				t.Fatalf("field %s/%s mutated collector: %+v, error %v", name, mode, execution, err)
			}
		}
		fields[name] = original
	}
}

func TestCodeCommentUnicodeLimitsAndEnums(t *testing.T) {
	for _, severity := range []model.Severity{model.SeverityHigh, model.SeverityMedium, model.SeverityLow} {
		for _, category := range []model.Category{model.CategoryBug, model.CategorySecurity, model.CategoryPerformance} {
			registry := commentRegistry(t, "sample.go", NewCollector())
			finding := commentFinding(t)
			finding.Severity, finding.Category = severity, category
			finding.Title = strings.Repeat("中", 120)
			finding.Description, finding.Suggestion = strings.Repeat("中", 2000), strings.Repeat("中", 2000)
			if !executeComment(t, registry, finding).OK {
				t.Fatal("legal Unicode boundary or enum rejected")
			}
		}
	}
	for _, field := range []string{"title", "description", "suggestion"} {
		collector := NewCollector()
		registry := commentRegistry(t, "sample.go", collector)
		finding := commentFinding(t)
		switch field {
		case "title":
			finding.Title = strings.Repeat("中", 121)
		case "description":
			finding.Description = strings.Repeat("中", 2001)
		case "suggestion":
			finding.Suggestion = strings.Repeat("中", 2001)
		}
		if executeComment(t, registry, finding).OK || len(collector.Findings()) != 0 {
			t.Fatalf("oversized %s accepted", field)
		}
	}
	// 直接调用具体工具也不能绕过枚举或文本校验。
	commenter, _ := NewCommenter(commentFixture("sample.go"), NewCollector())
	for _, value := range []string{"", "critical", "HIGH", " high ", "unknown"} {
		finding := commentFinding(t)
		finding.Severity = model.Severity(value)
		result, err := commenter.Execute(context.Background(), finding)
		if err != nil || result.OK {
			t.Fatalf("invalid severity %q accepted", value)
		}
	}
	for _, value := range []string{"", "style", "test", "Bug", " bug ", "unknown"} {
		finding := commentFinding(t)
		finding.Category = model.Category(value)
		result, err := commenter.Execute(context.Background(), finding)
		if err != nil || result.OK {
			t.Fatalf("invalid category %q accepted", value)
		}
	}
}

func TestCommenterCopiesTaskAnchorsAndValidatesBinding(t *testing.T) {
	change := commentFixture("sample.go")
	collector := NewCollector()
	commenter, err := NewCommenter(change, collector)
	if err != nil {
		t.Fatal(err)
	}
	delete(change.AddedLines, 4)
	change.AddedLines[3] = struct{}{}
	change.Path = "other.go"
	change.HeadContent[0] = 0
	finding := commentFinding(t)
	if result, err := commenter.Execute(context.Background(), finding); err != nil || !result.OK {
		t.Fatal("external mutation changed bound task")
	}
	finding.StartLine = 3
	if result, _ := commenter.Execute(context.Background(), finding); result.OK {
		t.Fatal("external mutation expanded anchors")
	}
	for _, mutate := range []func(*gitdiff.FileChange){
		func(c *gitdiff.FileChange) { c.Reviewable = false },
		func(c *gitdiff.FileChange) { c.SkipReason = gitdiff.SkipGenerated },
		func(c *gitdiff.FileChange) { c.AddedLines = nil },
		func(c *gitdiff.FileChange) { c.AddedLines[7] = struct{}{} },
		func(c *gitdiff.FileChange) { c.Path = "vendor/a.go" },
		func(c *gitdiff.FileChange) { c.HeadContent = nil },
	} {
		candidate := commentFixture("sample.go")
		mutate(&candidate)
		if tool, err := NewCommenter(candidate, collector); err == nil || tool != nil {
			t.Fatal("invalid task binding accepted")
		}
	}
	if tool, err := NewCommenter(commentFixture("sample.go"), nil); err == nil || tool != nil {
		t.Fatal("nil collector accepted")
	}
}

func TestCodeCommentCancellationPreservesExistingFindings(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	finding := commentFinding(t)
	executeComment(t, registry, finding)
	before := collector.Findings()
	commenter, _ := NewCommenter(commentFixture("sample.go"), collector)
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		var ctx context.Context
		var cancel context.CancelFunc
		if cause == context.Canceled {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		} else {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		finding.Title = "Another issue"
		result, err := commenter.Execute(ctx, finding)
		cancel()
		if !errors.Is(err, cause) || !reflect.DeepEqual(result, Execution{}) || !reflect.DeepEqual(before, collector.Findings()) {
			t.Fatalf("canceled comment mutated state: %+v, %v", result, err)
		}
	}
}
