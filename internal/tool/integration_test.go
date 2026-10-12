// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
)

// TestToolsOfflineDemo executes real tools without a model, client, or runner.
func TestToolsOfflineDemo(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	repo := t.TempDir()
	fileReadGit(t, repo, "init", "--template=", "-b", "main")
	for key, value := range map[string]string{
		"user.name": "Mini Review Test", "user.email": "mini-review@example.com",
		"commit.gpgsign": "false", "core.autocrlf": "false",
		"core.hooksPath": filepath.Join(repo, "no-hooks"),
	} {
		fileReadGit(t, repo, "config", key, value)
	}
	const baseContent = "package sample\n\nfunc Sum(a, b int) int {\n\treturn a - b\n}\n"
	const headContent = "package sample\n\nfunc Sum(a, b int) int {\n\t// Add both inputs.\n\treturn a + b\n}\n"
	const relatedContent = "package sample\n\nconst MaxInput = 100\n"
	writeReadFixture(t, repo, "sample.go", baseContent)
	writeReadFixture(t, repo, "related.go", relatedContent)
	fileReadGit(t, repo, "add", "-A")
	fileReadGit(t, repo, "commit", "-m", "Base offline fixture")
	base := fileReadGit(t, repo, "rev-parse", "HEAD")
	writeReadFixture(t, repo, "sample.go", headContent)
	fileReadGit(t, repo, "add", "sample.go")
	// Construct a link tree entry without requiring OS symlink permissions.
	blob := fileReadGit(t, repo, "rev-parse", base+":related.go")
	fileReadGit(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+blob+",link.go")
	fileReadGit(t, repo, "commit", "-m", "Head offline fixture")
	head := fileReadGit(t, repo, "rev-parse", "HEAD")

	writeReadFixture(t, repo, "sample.go", "package staged\n")
	fileReadGit(t, repo, "add", "sample.go")
	writeReadFixture(t, repo, "sample.go", "package unstaged\n")
	if err := os.Remove(filepath.Join(repo, "related.go")); err != nil {
		t.Fatal(err)
	}
	beforeStatus := fileReadGit(t, repo, "status", "--porcelain")
	beforeIndex := fileReadGit(t, repo, "diff", "--cached")
	if beforeStatus == "" || beforeIndex == "" {
		t.Fatal("fixture must contain staged and worktree changes")
	}

	ctx := context.Background()
	snapshot, err := gitdiff.Resolve(ctx, repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != base || snapshot.HeadSHA != head {
		t.Fatalf("resolved commits drifted: %+v", snapshot)
	}
	set, err := gitdiff.Load(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if set.Snapshot != snapshot || set.HasCoverageGap || len(set.Warnings) != 0 || len(set.Files) != 2 {
		t.Fatalf("unexpected loaded change set: %+v", set)
	}
	var current gitdiff.FileChange
	for _, change := range set.Files {
		switch change.Path {
		case "sample.go":
			current = change
		case "link.go":
			if change.Reviewable || change.SkipReason != gitdiff.SkipSpecialFile {
				t.Fatalf("link must be excluded: %+v", change)
			}
		default:
			t.Fatalf("unchanged related file appeared in diff: %s", change.Path)
		}
	}
	if !current.Reviewable || string(current.HeadContent) != headContent ||
		!reflect.DeepEqual(current.AddedLines, map[int]struct{}{4: {}, 5: {}}) {
		t.Fatalf("unexpected current file or anchors: %+v", current)
	}
	reader, err := NewFileReader(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	collector := NewCollector()
	commenter, err := NewCommenter(current, collector)
	if err != nil {
		t.Fatal(err)
	}
	finisher, err := NewTaskFinisher(current.Path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(Handlers{FileRead: reader.Execute, CodeComment: commenter.Execute, TaskDone: finisher.Execute})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(name string, arguments json.RawMessage) Execution {
		t.Helper()
		result, err := registry.Execute(ctx, llm.ToolCall{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("execute %s: %v", name, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("tool=%s fixed_head=%s result=%s collector_count=%d completion=%+v", name, head, encoded, len(collector.Findings()), result.Completion)
		if name != TaskDone && result.Completion != nil {
			t.Fatalf("%s produced a completion declaration", name)
		}
		return result
	}
	marshal := func(value any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, path := range []string{current.Path, "related.go"} {
		result := execute(FileRead, marshal(FileReadArgs{Path: path}))
		data, ok := result.Data.(FileReadData)
		if !result.OK || !ok || data.HeadSHA != head || data.Path != path || data.StartLine == nil ||
			*data.StartLine != 1 || data.EndLine == nil || data.Truncated || data.LastLineTruncated {
			t.Fatalf("invalid fixed head read: %+v", result)
		}
		content := relatedContent
		if path == current.Path {
			content = headContent
		}
		lines := SplitSourceLines([]byte(content))
		if data.TotalLines != len(lines) || *data.EndLine != len(lines) {
			t.Fatalf("incorrect actual line range: %+v", data)
		}
		var numbered string
		for i, line := range lines {
			numbered += fmt.Sprintf("%d|%s\n", i+1, line)
		}
		if data.Content != numbered {
			t.Fatalf("numbered content differs from fixed head: %+v", data)
		}
	}
	finding := model.Finding{
		Path: current.Path, StartLine: 5, EndLine: 6,
		Severity: model.SeverityLow, Category: model.CategoryBug,
		Title: "Integer sum may overflow", Description: "Large integer inputs can overflow and produce an incorrect sum.",
		Suggestion: "Check input bounds before adding the values.",
	}
	if _, added := current.AddedLines[finding.StartLine]; !added {
		t.Fatal("comment start must be an added head line")
	}
	if _, added := current.AddedLines[finding.EndLine]; added {
		t.Fatal("comment end must exercise an unchanged context line")
	}
	checkFailures := func() {
		t.Helper()
		before := collector.Findings()
		other, contextFinding := finding, finding
		other.Path = "related.go"
		contextFinding.StartLine = 3
		for _, call := range []struct {
			name string
			args json.RawMessage
			code string
		}{
			{"unknown", json.RawMessage(`{}`), "unknown_tool"},
			{CodeComment, json.RawMessage(`{"path":`), "invalid_arguments"},
			{FileRead, marshal(FileReadArgs{Path: "missing.go"}), "read_failed"},
			{FileRead, marshal(FileReadArgs{Path: "link.go"}), "special_file"},
			{CodeComment, marshal(other), "wrong_file"},
			{CodeComment, marshal(contextFinding), "invalid_anchor"},
		} {
			result := execute(call.name, call.args)
			if result.OK || result.Error == nil || result.Error.Code != call.code || result.Data != nil ||
				result.Completion != nil || !reflect.DeepEqual(collector.Findings(), before) {
				t.Fatalf("failure changed collector or returned wrong result: %+v", result)
			}
		}
	}
	checkFailures()
	first := execute(CodeComment, marshal(finding))
	if !first.OK || first.Data != (CommentData{Accepted: true}) || !reflect.DeepEqual(collector.Findings(), []model.Finding{finding}) {
		t.Fatalf("first comment was not collected: %+v", first)
	}
	duplicate := execute(CodeComment, marshal(finding))
	if !duplicate.OK || duplicate.Data != (CommentData{Accepted: true, Duplicate: true}) ||
		!reflect.DeepEqual(collector.Findings(), []model.Finding{finding}) {
		t.Fatalf("duplicate changed collector: %+v", duplicate)
	}
	checkFailures()
	const summary = "Checked fixed head arithmetic, related context, and comment anchors."
	done := execute(TaskDone, marshal(TaskDoneArgs{Summary: summary}))
	if !done.OK || done.Data != (TaskDoneData{DoneRequested: true, Path: current.Path, Summary: summary}) ||
		done.Completion == nil || *done.Completion != (Completion{Path: current.Path, Summary: summary}) ||
		!reflect.DeepEqual(collector.Findings(), []model.Finding{finding}) {
		t.Fatalf("invalid single-file completion declaration: %+v", done)
	}
	if fileReadGit(t, repo, "status", "--porcelain") != beforeStatus || fileReadGit(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("offline tools modified worktree or index")
	}
	t.Logf("fixed_head=%s collector_count=1 worktree_and_index_unchanged=true runner_executed=false", head)
}
