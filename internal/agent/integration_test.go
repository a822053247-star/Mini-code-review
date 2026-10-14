// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

// Keep the minimal Git fixture inside this test package, independent of the
// unexported helpers in gitdiff and tool. All writes target t.TempDir().
func offlineDemoGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func offlineDemoWrite(t *testing.T, repo, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerOfflineDemo(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("LC_ALL", "C")
	repo := t.TempDir()
	offlineDemoGit(t, repo, "init", "--template=", "-b", "main")
	for key, value := range map[string]string{
		"user.name": "Mini Review Test", "user.email": "mini-review@example.com",
		"commit.gpgsign": "false", "core.autocrlf": "false", "core.hooksPath": filepath.Join(repo, "no-hooks"),
	} {
		offlineDemoGit(t, repo, "config", key, value)
	}
	const baseContent = "package sample\n\nfunc Sum(a, b int) int {\n\treturn a - b\n}\n"
	const headContent = "package sample\n\nfunc Sum(a, b int) int {\n\t// Add both inputs.\n\treturn a + b\n}\n"
	const relatedContent = "package sample\n\nconst MaxInput = 100\n"
	offlineDemoWrite(t, repo, "sample.go", baseContent)
	offlineDemoWrite(t, repo, "related.go", relatedContent)
	offlineDemoGit(t, repo, "add", "-A")
	offlineDemoGit(t, repo, "commit", "-m", "Base offline runner fixture")
	base := strings.TrimSpace(offlineDemoGit(t, repo, "rev-parse", "HEAD"))
	offlineDemoWrite(t, repo, "sample.go", headContent)
	offlineDemoGit(t, repo, "add", "sample.go")
	offlineDemoGit(t, repo, "commit", "-m", "Head offline runner fixture")
	head := strings.TrimSpace(offlineDemoGit(t, repo, "rev-parse", "HEAD"))
	ctx := context.Background()
	snapshot, err := gitdiff.Resolve(ctx, repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != base || snapshot.HeadSHA != head || strings.TrimSpace(offlineDemoGit(t, repo, "rev-list", "--count", "HEAD")) != "2" {
		t.Fatalf("fixture must pin exactly two commits: %+v", snapshot)
	}
	// Leave different staged/worktree source and remove the related worktree
	// file. Resolve, Load, and production file_read must use committed objects.
	offlineDemoWrite(t, repo, "sample.go", "package staged\n")
	offlineDemoGit(t, repo, "add", "sample.go")
	offlineDemoWrite(t, repo, "sample.go", "package unstaged\n")
	if err := os.Remove(filepath.Join(repo, "related.go")); err != nil {
		t.Fatal(err)
	}
	beforeStatus := offlineDemoGit(t, repo, "status", "--porcelain")
	beforeIndex := offlineDemoGit(t, repo, "diff", "--cached")
	if !strings.Contains(beforeStatus, "MM sample.go") || !strings.Contains(beforeStatus, " D related.go") || beforeIndex == "" {
		t.Fatal("fixture must contain staged, unstaged, and deleted worktree source")
	}
	set, err := gitdiff.Load(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if set.Snapshot != snapshot || set.HasCoverageGap || len(set.Warnings) != 0 || len(set.Files) != 1 ||
		set.Files[0].Path != "sample.go" || !set.Files[0].Reviewable || string(set.Files[0].HeadContent) != headContent ||
		!reflect.DeepEqual(set.Files[0].AddedLines, map[int]struct{}{4: {}, 5: {}}) {
		t.Fatalf("unexpected loaded comparison: %+v", set)
	}
	finding := model.Finding{
		Path: "sample.go", StartLine: 5, EndLine: 6, Severity: model.SeverityLow, Category: model.CategoryBug,
		Title: "Integer sum may overflow", Description: "Large integer inputs can overflow and produce an incorrect sum.",
		Suggestion: "Check input bounds before addition.",
	}
	badAnchor := finding
	badAnchor.StartLine = 3
	call := func(id, name string, args any) llm.ToolCall {
		t.Helper()
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return llm.ToolCall{ID: id, Name: name, Arguments: encoded}
	}
	step := func(calls ...llm.ToolCall) llm.MockStep {
		return llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}}}
	}
	readRelated := call("read_related", tool.FileRead, tool.FileReadArgs{Path: "related.go"})
	readCurrent := call("read_current", tool.FileRead, tool.FileReadArgs{Path: "sample.go"})
	accepted := call("accepted", tool.CodeComment, finding)
	duplicate := call("duplicate", tool.CodeComment, finding)
	done := call("done", tool.TaskDone, tool.TaskDoneArgs{Summary: "Checked arithmetic and related context."})
	for _, scenario := range []struct {
		name  string
		steps []llm.MockStep
		codes map[string]string
	}{
		{"success", []llm.MockStep{step(readRelated, readCurrent), step(accepted, duplicate), step(done)}, nil},
		{"recovery", []llm.MockStep{
			step(call("rejected_done", tool.TaskDone, tool.TaskDoneArgs{Summary: "Checked current file."}),
				call("bad_anchor", tool.CodeComment, badAnchor), readRelated, readCurrent, accepted, duplicate),
			step(call("corrected_anchor", tool.CodeComment, finding), call("renewed_done", tool.TaskDone, tool.TaskDoneArgs{Summary: "Corrected the anchor and rechecked the current file."})),
		}, map[string]string{"bad_anchor": "invalid_anchor"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			client := llm.NewMockClient(scenario.steps)
			// A nil read dependency selects real Git head-object reads.
			agent, err := NewAgent(client, RunnerOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := agent.Run(ctx, set, "Check arithmetic using related constants.")
			if err != nil {
				t.Fatal(err)
			}
			wantFile := model.FileResult{Path: "sample.go", Status: model.FileCompleted, StopReason: "task_done", Rounds: len(scenario.steps)}
			wantTools := 0
			for _, scripted := range scenario.steps {
				wantTools += len(scripted.Response.Assistant.ToolCalls)
			}
			if result.Snapshot != snapshot || !reflect.DeepEqual(result.Files, []model.FileResult{wantFile}) ||
				!reflect.DeepEqual(result.Findings, []model.Finding{finding}) || result.HasCoverageGap || len(result.Warnings) != 0 ||
				len(result.FileErrors) != 0 || result.Usage.LLMRequests != len(scenario.steps) || result.Usage.ToolCalls != wantTools ||
				result.Usage.InputTokens != nil || result.Usage.OutputTokens != nil {
				t.Fatalf("unexpected offline outcome: %+v", result)
			}
			requests := client.Requests()
			initial, err := BuildInitialMessages(set, "sample.go", "Check arithmetic using related constants.")
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != len(scenario.steps) || !reflect.DeepEqual(requests[0].Messages, initial) {
				t.Fatal("runner did not start with the loaded file context")
			}
			if len(requests[0].Tools) != 3 || requests[0].Tools[0].Name != tool.FileRead || requests[0].Tools[1].Name != tool.CodeComment || requests[0].Tools[2].Name != tool.TaskDone {
				t.Fatal("model request did not include all real tool definitions")
			}
			t.Logf("scenario=%s fixed_base=%s fixed_head=%s current_file=sample.go added_lines=[4 5] initial_roles=system,user", scenario.name, base, head)
			for round := 1; round < len(requests); round++ {
				previous := requests[round-1]
				current := requests[round]
				assistant := scenario.steps[round-1].Response.Assistant
				offset := len(previous.Messages)
				if len(current.Messages) != offset+1+len(assistant.ToolCalls) ||
					!reflect.DeepEqual(current.Messages[:offset], previous.Messages) ||
					!reflect.DeepEqual(current.Messages[offset], assistant) || !reflect.DeepEqual(current.Tools, requests[0].Tools) {
					t.Fatalf("request round %d lost assistant calls, raw arguments, or earlier history", round+1)
				}
				for index, executed := range assistant.ToolCalls {
					message := current.Messages[offset+1+index]
					encoded := assertToolMessage(t, message, executed.ID, scenario.codes[executed.ID])
					duplicateFlag := false
					if data, ok := encoded["data"].(map[string]any); ok {
						duplicateFlag, _ = data["duplicate"].(bool)
						if executed.ID == "duplicate" && (!duplicateFlag || data["accepted"] != true) {
							t.Fatal("duplicate submission did not return a successful duplicate result")
						}
						if executed.ID == "accepted" && (duplicateFlag || data["accepted"] != true) {
							t.Fatal("first valid finding was not accepted as a new submission")
						}
						if executed.Name == tool.TaskDone && (data["done_requested"] != true || data["path"] != "sample.go") {
							t.Fatal("completion result lost its current-file binding")
						}
					}
					if executed.Name == tool.FileRead {
						var envelope struct {
							Data tool.FileReadData `json:"data"`
						}
						if err := json.Unmarshal([]byte(message.Content), &envelope); err != nil {
							t.Fatal(err)
						}
						data := envelope.Data
						wantPath, wantLines := "related.go", 3
						wantContent := "1|package sample\n2|\n3|const MaxInput = 100\n"
						if executed.ID == "read_current" {
							wantPath, wantLines = "sample.go", 6
							wantContent = "1|package sample\n2|\n3|func Sum(a, b int) int {\n4|\t// Add both inputs.\n5|\treturn a + b\n6|}\n"
						}
						if data.Path != wantPath || data.HeadSHA != head || data.Content != wantContent || data.TotalLines != wantLines ||
							data.StartLine == nil || *data.StartLine != 1 || data.EndLine == nil || *data.EndLine != wantLines || data.Truncated || data.LastLineTruncated {
							t.Fatalf("read followed worktree or lost actual head line numbers: %+v", data)
						}
					}
					if executed.ID == "rejected_done" {
						t.Logf("round=%d tool=%s tool_call_id=%s completion_accepted=false reason=same_round_tool_error", round, executed.Name, executed.ID)
					}
					t.Logf("round=%d fixed_head=%s tool=%s tool_call_id=%s ok=%v error_code=%q duplicate=%t result=%s", round, head, executed.Name, executed.ID, encoded["ok"], scenario.codes[executed.ID], duplicateFlag, message.Content)
				}
			}
			// A completed final batch causes no extra model request. Its acceptance
			// is observed through FileResult, not a fabricated final tool message.
			for _, executed := range scenario.steps[len(scenario.steps)-1].Response.Assistant.ToolCalls {
				t.Logf("round=%d fixed_head=%s tool=%s tool_call_id=%s final_batch_completed=true", wantFile.Rounds, head, executed.Name, executed.ID)
			}
			t.Logf("scenario=%s collector_count=%d file_status=%s stop_reason=%s rounds=%d llm_requests=%d tool_calls=%d", scenario.name, len(result.Findings), wantFile.Status, wantFile.StopReason, wantFile.Rounds, result.Usage.LLMRequests, result.Usage.ToolCalls)
			if offlineDemoGit(t, repo, "status", "--porcelain") != beforeStatus || offlineDemoGit(t, repo, "diff", "--cached") != beforeIndex {
				t.Fatal("offline review modified worktree or index")
			}
			t.Logf("scenario=%s worktree_and_index_unchanged=true report_written=false trace_written=false", scenario.name)
		})
	}
}
