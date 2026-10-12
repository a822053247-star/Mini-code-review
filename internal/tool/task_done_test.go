// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"example.com/mini-code-review/internal/llm"
)

func TestTaskDoneDeclaresOnlyBoundFileWithoutChangingCollector(t *testing.T) {
	collector := NewCollector()
	first := commentRegistry(t, "sample.go", collector)
	second := commentRegistry(t, "other.go", collector)
	for _, stage := range []string{"empty", "with finding"} {
		if stage == "with finding" {
			executeComment(t, first, commentFinding(t))
		}
		before := collector.Findings()
		for _, tc := range []struct {
			registry *Registry
			path     string
		}{{first, "sample.go"}, {second, "other.go"}} {
			result, err := tc.registry.Execute(context.Background(), llm.ToolCall{ID: "done_id", Name: TaskDone, Arguments: json.RawMessage(`{"summary":"  Checked boundary cases. \n"}`)})
			want := TaskDoneData{DoneRequested: true, Path: tc.path, Summary: "Checked boundary cases."}
			if err != nil || !result.OK || result.Data != want || result.Completion == nil || *result.Completion != (Completion{Path: tc.path, Summary: want.Summary}) || !reflect.DeepEqual(before, collector.Findings()) {
				t.Fatalf("invalid done declaration: %+v, %v", result, err)
			}
			encoded, err := json.Marshal(result)
			var envelope struct {
				OK   bool         `json:"ok"`
				Data TaskDoneData `json:"data"`
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil || !envelope.OK || envelope.Data != want {
				t.Fatalf("invalid done JSON: %s", encoded)
			}
			// 修改返回的完成信号不会改变执行器绑定的文件。
			result.Completion.Path = "changed.go"
		}
	}
	// 能读取关联文件，但不能把它变成当前文件的评论目标。
	read, err := first.Execute(context.Background(), llm.ToolCall{Name: FileRead, Arguments: json.RawMessage(`{"path":"other.go"}`)})
	if err != nil || !read.OK {
		t.Fatal("related read failed")
	}
	otherFinding := commentFinding(t)
	otherFinding.Path = "other.go"
	before := collector.Findings()
	if result := executeComment(t, first, otherFinding); result.Error == nil || result.Error.Code != "wrong_file" || !reflect.DeepEqual(before, collector.Findings()) {
		t.Fatal("related read expanded comment scope")
	}
	if !executeComment(t, second, otherFinding).OK || len(collector.Findings()) != 2 {
		t.Fatal("second task did not retain independent scope")
	}
}

func TestTaskDoneRejectsBadSummaryWithoutSideEffects(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	executeComment(t, registry, commentFinding(t))
	before := collector.Findings()
	for _, raw := range []string{`{}`, `{"summary":null}`, `{"summary":""}`, `{"summary":" \t\n"}`, `{"summary":1}`, `{"summary":"Checked","path":"other.go"}`, `{"summary":"Checked"} {}`} {
		result, err := registry.Execute(context.Background(), llm.ToolCall{Name: TaskDone, Arguments: json.RawMessage(raw)})
		if err != nil || result.OK || result.Error == nil || result.Error.Code != "invalid_arguments" || result.Completion != nil || !reflect.DeepEqual(before, collector.Findings()) {
			t.Fatalf("bad summary %s: %+v, %v", raw, result, err)
		}
	}
	finisher, err := NewTaskFinisher("sample.go")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := finisher.Execute(context.Background(), TaskDoneArgs{Summary: " \n"}); err != nil || result.OK || result.Completion != nil {
		t.Fatal("direct call bypassed summary validation")
	}
	if finisher, err := NewTaskFinisher("../other.go"); err == nil || finisher != nil {
		t.Fatal("invalid bound file accepted")
	}
}

func TestTaskDonePropagatesCancellation(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	executeComment(t, registry, commentFinding(t))
	before := collector.Findings()
	finisher, _ := NewTaskFinisher("sample.go")
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		var ctx context.Context
		var cancel context.CancelFunc
		if cause == context.Canceled {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		} else {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}
		result, err := finisher.Execute(ctx, TaskDoneArgs{Summary: "Checked."})
		if !errors.Is(err, cause) || !reflect.DeepEqual(result, Execution{}) {
			t.Fatalf("direct canceled done: %+v, %v", result, err)
		}
		result, err = registry.Execute(ctx, llm.ToolCall{Name: TaskDone, Arguments: json.RawMessage(`{"summary":"Checked."}`)})
		cancel()
		if !errors.Is(err, cause) || !reflect.DeepEqual(result, Execution{}) || !reflect.DeepEqual(before, collector.Findings()) {
			t.Fatalf("registered canceled done: %+v, %v", result, err)
		}
	}
}
