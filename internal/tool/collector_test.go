// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCollectorDeduplicatesAndKeepsFirstFinding(t *testing.T) {
	collector := NewCollector()
	registry := commentRegistry(t, "sample.go", collector)
	first := commentFinding(t)
	first.Title = "Wrong   RESULT"
	executeComment(t, registry, first)
	duplicate := first
	duplicate.Title = " wrong\tresult "
	duplicate.Description = "Different description must not replace the first."
	duplicate.Suggestion = "Different suggestion."
	result := executeComment(t, registry, duplicate)
	if !result.OK || result.Data != (CommentData{Accepted: true, Duplicate: true}) || !reflect.DeepEqual(collector.Findings(), []CodeCommentArgs{first}) {
		t.Fatalf("duplicate replaced first finding: %+v", result)
	}
	for _, mutate := range []func(*CodeCommentArgs){
		func(f *CodeCommentArgs) { f.StartLine = 5 },
		func(f *CodeCommentArgs) { f.EndLine = 6 },
		func(f *CodeCommentArgs) { f.Title = "Other title" },
		func(f *CodeCommentArgs) { f.Title = "Wrong RESULT!" },
	} {
		finding := first
		mutate(&finding)
		result := executeComment(t, registry, finding)
		if result.Data != (CommentData{Accepted: true}) {
			t.Fatalf("distinct finding merged: %+v", result)
		}
	}
	other := commentRegistry(t, "other.go", collector)
	finding := first
	finding.Path = "other.go"
	if !executeComment(t, other, finding).OK || len(collector.Findings()) != 6 {
		t.Fatal("distinct file merged")
	}
	if collector.Findings()[0] != first || collector.Findings()[5] != finding {
		t.Fatal("collector order changed")
	}
}

func TestCollectorReturnsIndependentNonNilSlices(t *testing.T) {
	var collector Collector
	if collector.Findings() == nil {
		t.Fatal("empty findings must be non-nil")
	}
	encoded, err := json.Marshal(collector.Findings())
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("empty findings: %s, %v", encoded, err)
	}
	registry := commentRegistry(t, "sample.go", &collector)
	first := commentFinding(t)
	executeComment(t, registry, first)
	copy := collector.Findings()
	copy[0].Title = "Changed externally"
	if collector.Findings()[0] != first {
		t.Fatal("returned slice changed collector")
	}
	if result := executeComment(t, registry, first); result.Data != (CommentData{Accepted: true, Duplicate: true}) {
		t.Fatal("returned slice changed dedup state")
	}
	separate := NewCollector()
	if result := executeComment(t, commentRegistry(t, "sample.go", separate), first); result.Data != (CommentData{Accepted: true}) {
		t.Fatal("separate runs shared dedup state")
	}
}
