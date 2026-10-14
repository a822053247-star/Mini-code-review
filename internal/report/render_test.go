// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/agent"
	"example.com/mini-code-review/internal/model"
)

func TestBuildRunStatusRules(t *testing.T) {
	complete := model.FileResult{Path: "a.go", Status: model.FileCompleted, StopReason: "task_done"}
	failed := model.FileResult{Path: "b.go", Status: model.FileFailed, StopReason: "max_rounds"}
	skipped := model.FileResult{Path: "notes.md", Status: model.FileSkipped, StopReason: "unsupported_extension"}
	for _, tc := range []struct {
		name                string
		files               []model.FileResult
		gap, finding, fatal bool
		want                model.RunStatus
	}{
		{"empty", nil, false, false, false, model.RunSkipped}, {"static skipped", []model.FileResult{skipped}, false, false, false, model.RunSkipped},
		{"complete zero findings", []model.FileResult{complete, skipped}, false, false, false, model.RunCompleted},
		{"complete with findings", []model.FileResult{complete}, false, true, false, model.RunCompleted},
		{"partial files", []model.FileResult{complete, failed}, false, false, false, model.RunPartial},
		{"partial findings", []model.FileResult{failed}, true, true, false, model.RunPartial},
		{"all failed", []model.FileResult{failed}, true, false, false, model.RunFailed},
		{"only size skipped", []model.FileResult{skipped}, true, false, false, model.RunFailed},
		{"completed plus size gap", []model.FileResult{complete, skipped}, true, false, false, model.RunPartial},
		{"fatal overrides progress", []model.FileResult{complete}, false, true, true, model.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := agent.ReviewResult{Files: tc.files, HasCoverageGap: tc.gap}
			if tc.finding {
				review.Findings = []model.Finding{{Path: "a.go", StartLine: 4}}
			}
			result := Build(review, 12*time.Millisecond, tc.fatal)
			if result.Status != tc.want || result.DurationMS != 12 || result.Files == nil || result.Findings == nil || result.Warnings == nil {
				t.Fatalf("wrong status or empty collections: %+v", result)
			}
		})
	}
}

func TestRenderJSONAndIncompleteMarkdown(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunCompleted, model.RunPartial, model.RunFailed, model.RunSkipped} {
		result := model.Report{Status: status}
		encoded, err := Render(result, "json")
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fields["findings"], []any{}) || fields["usage"].(map[string]any)["input_tokens"] != nil {
			t.Fatal("JSON confused empty or unknown values")
		}
		markdown, err := Render(result, "markdown")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(markdown), "No findings reported in this completed review.") != (status == model.RunCompleted) {
			t.Fatal("incomplete run claimed complete zero-issue review")
		}
		if (status == model.RunPartial || status == model.RunFailed) && !strings.Contains(string(markdown), "Review incomplete") {
			t.Fatal("missing incomplete warning")
		}
	}
	if _, err := Render(model.Report{}, "text"); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestReportCopiesValuesAndEscapesModelText(t *testing.T) {
	zero := int64(0)
	review := agent.ReviewResult{Files: []model.FileResult{{Path: "a.go", Status: model.FileCompleted}}, Findings: []model.Finding{{Path: "a.go", StartLine: 4, EndLine: 5, Title: "<script>|title", Description: "cause\nimpact", Suggestion: "repair"}}, Warnings: []string{"warning"}, Usage: model.Usage{InputTokens: &zero}}
	result := Build(review, 0, false)
	result.Files[0].Path = "changed"
	*result.Usage.InputTokens = 9
	if review.Files[0].Path != "a.go" || zero != 0 {
		t.Fatal("report leaked mutable values")
	}
	encoded, _ := Render(result, "markdown")
	if strings.Contains(string(encoded), "<script>") || !strings.Contains(string(encoded), "a.go:4") || !strings.Contains(string(encoded), "Cause and impact: cause impact") {
		t.Fatal("finding location or escaping failed")
	}
}
