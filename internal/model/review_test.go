// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReportJSONContract(t *testing.T) {
	report := Report{
		Status:  RunCompleted,
		BaseSHA: "base-sha",
		HeadSHA: "head-sha",
		Files: []FileResult{{
			Path: "internal/service.go", Status: FileCompleted,
			StopReason: "task_done", Rounds: 3,
		}},
		Findings: []Finding{{
			Path: "internal/service.go", StartLine: 42, EndLine: 42,
			Severity: SeverityHigh, Category: CategoryBug,
			Title:       "Nil result is dereferenced",
			Description: "A failed lookup returns nil and accessing result.ID panics.",
			Suggestion:  "Handle the lookup error before accessing result.ID.",
		}},
		Warnings:   []string{},
		Usage:      Usage{LLMRequests: 3, ToolCalls: 3},
		DurationMS: 1850,
	}
	assertJSONContract(t, report, `{
		"status":"completed", "base_sha":"base-sha", "head_sha":"head-sha",
		"files":[{"path":"internal/service.go","status":"completed","stop_reason":"task_done","rounds":3}],
		"findings":[{
			"path":"internal/service.go","start_line":42,"end_line":42,
			"severity":"high","category":"bug","title":"Nil result is dereferenced",
			"description":"A failed lookup returns nil and accessing result.ID panics.",
			"suggestion":"Handle the lookup error before accessing result.ID."
		}],
		"warnings":[],
		"usage":{"llm_requests":3,"tool_calls":3,"input_tokens":null,"output_tokens":null},
		"duration_ms":1850
	}`)
}

func TestReportEmptyCollectionsAndKnownZeroTokens(t *testing.T) {
	zero := int64(0)
	report := Report{
		Status:   RunSkipped,
		Files:    []FileResult{},
		Findings: []Finding{},
		Warnings: []string{},
		Usage:    Usage{InputTokens: &zero, OutputTokens: &zero},
	}
	assertJSONContract(t, report, `{
		"status":"skipped", "base_sha":"", "head_sha":"",
		"files":[], "findings":[], "warnings":[],
		"usage":{"llm_requests":0,"tool_calls":0,"input_tokens":0,"output_tokens":0},
		"duration_ms":0
	}`)
}

func assertJSONContract(t *testing.T, value any, expected string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON contract mismatch:\ngot: %s\nwant: %s", encoded, expected)
	}
}
