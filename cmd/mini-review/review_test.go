// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/trace"
)

func cliGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git: %v\n%s", err, output)
	}
	return string(output)
}

func cliRepo(t *testing.T, head map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	repo := t.TempDir()
	cliGit(t, repo, "init", "--template=", "-b", "main")
	for key, value := range map[string]string{"user.name": "Mini Review Test", "user.email": "mini-review@example.com", "commit.gpgsign": "false", "core.autocrlf": "false", "core.hooksPath": filepath.Join(repo, "no-hooks")} {
		cliGit(t, repo, "config", key, value)
	}
	for path := range head {
		content := "base\n"
		if strings.HasSuffix(path, ".go") {
			content = "package sample\n// base\n"
		}
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cliGit(t, repo, "add", "-A")
	cliGit(t, repo, "commit", "-m", "Base CLI fixture")
	for path, content := range head {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cliGit(t, repo, "add", "-A")
	cliGit(t, repo, "commit", "-m", "Head CLI fixture")
	return repo
}

func TestCLIReportsAndTraceOffline(t *testing.T) {
	repo := cliRepo(t, map[string]string{"sample.go": "package sample\n// changed\n", "notes.md": "changed docs\n"})
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	var stdout, stderr bytes.Buffer
	code := runCommand(context.Background(), []string{"--repo", repo, "--mock", "--format=json", "--trace", tracePath}, &stdout, &stderr, nil)
	var result model.Report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || result.Status != model.RunCompleted || len(result.Findings) != 1 || result.Usage.LLMRequests != 3 || result.Usage.ToolCalls != 3 || result.Usage.InputTokens != nil || stderr.Len() != 0 {
		t.Fatalf("wrong CLI report/exit: %d %+v %s", code, result, stderr.String())
	}
	if result.Findings[0].StartLine != 2 || !strings.Contains(result.Warnings[0], "scripted") {
		t.Fatal("mock finding lost anchor or disclosure")
	}
	body, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	events := []trace.Event{}
	names := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var event trace.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		names[event.Event]++
		if event.RunID != events[0].RunID || event.Time == "" {
			t.Fatal("trace identity/time mismatch")
		}
	}
	for _, name := range []string{"run_start", "file_start", "llm_start", "llm_end", "tool_start", "tool_end", "file_end", "run_end"} {
		if names[name] == 0 {
			t.Fatalf("missing trace event %s", name)
		}
	}
	if events[len(events)-1].Status != string(result.Status) || names["llm_start"] != result.Usage.LLMRequests || names["tool_start"] != result.Usage.ToolCalls {
		t.Fatal("trace and report disagree")
	}
	if strings.Contains(string(body), "package sample") || strings.Contains(string(body), "scripted finding") || strings.Contains(string(body), "arguments") {
		t.Fatal("trace exposed code or payload")
	}
	t.Logf("report_status=%s findings=%d llm_requests=%d tool_calls=%d trace_events=%d exit=%d", result.Status, len(result.Findings), result.Usage.LLMRequests, result.Usage.ToolCalls, len(events), code)
}

func TestCLIOutputFileAndPreflight(t *testing.T) {
	repo := cliRepo(t, map[string]string{"sample.go": "package sample\n// changed\n"})
	root := t.TempDir()
	output := filepath.Join(root, "review.md")
	tracePath := filepath.Join(root, "trace.jsonl")
	var stdout, stderr bytes.Buffer
	if code := runCommand(context.Background(), []string{"--repo", repo, "--mock", "--output", output, "--trace", tracePath}, &stdout, &stderr, nil); code != 0 || stdout.Len() != 0 {
		t.Fatalf("file output polluted stdout or failed: %d %s", code, stderr.String())
	}
	body, _ := os.ReadFile(output)
	if !strings.Contains(string(body), "sample.go:2") {
		t.Fatal("Markdown report lost location")
	}
	called := 0
	factory := func(gitdiff.ChangeSet) (llm.Client, error) { called++; return nil, errors.New("should not be reached") }
	for _, args := range [][]string{
		{"--output", output}, {"--trace", tracePath}, {"--output", filepath.Join(root, "same"), "--trace", filepath.Join(root, "same")},
		{"--output", filepath.Join(root, "new.md"), "--trace", tracePath}, {"--output", filepath.Join(root, "missing", "report.json")},
	} {
		stdout.Reset()
		stderr.Reset()
		full := append([]string{"--repo", repo, "--mock"}, args...)
		if code := runCommand(context.Background(), full, &stdout, &stderr, factory); code != 1 || stdout.Len() != 0 || called != 0 {
			t.Fatal("preflight allowed model request or overwrote output")
		}
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(body, after) {
		t.Fatal("existing report was overwritten")
	}
	if _, err := os.Stat(filepath.Join(root, "new.md")); !os.IsNotExist(err) {
		t.Fatal("failed preflight left a reserved report")
	}
}

func TestCLIPartialAndFailedReports(t *testing.T) {
	repo := cliRepo(t, map[string]string{"a.go": "package sample\n// changed\n", "z.go": "package sample\n// changed\n"})
	for _, completeFirst := range []bool{false, true} {
		tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
		factory := func(gitdiff.ChangeSet) (llm.Client, error) {
			steps := []llm.MockStep{}
			if completeFirst {
				steps = append(steps, llm.MockStep{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "done_a", Name: "task_done", Arguments: json.RawMessage(`{"summary":"Checked a.go."}`)}}}}})
			} else {
				steps = append(steps, llm.MockStep{Error: errors.New("private backend payload")})
			}
			steps = append(steps, llm.MockStep{Error: errors.New("private backend payload")})
			return llm.NewMockClient(steps), nil
		}
		var stdout, stderr bytes.Buffer
		code := runCommand(context.Background(), []string{"--repo", repo, "--format=json", "--trace", tracePath}, &stdout, &stderr, factory)
		var result model.Report
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want := model.RunFailed
		if completeFirst {
			want = model.RunPartial
		}
		if code != 1 || result.Status != want || result.Usage.LLMRequests != 2 || strings.Contains(stderr.String(), "private backend payload") {
			t.Fatalf("wrong incomplete outcome: %d %+v %s", code, result, stderr.String())
		}
		body, _ := os.ReadFile(tracePath)
		if strings.Contains(string(body), "private backend payload") {
			t.Fatal("trace leaked raw model failure")
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		var end trace.Event
		json.Unmarshal([]byte(lines[len(lines)-1]), &end)
		if end.Status != string(want) {
			t.Fatal("incomplete trace disagrees with report")
		}
		t.Logf("report_status=%s llm_requests=%d exit=%d", result.Status, result.Usage.LLMRequests, code)
	}
}

func TestCLINoApplicableAndSizeSkipped(t *testing.T) {
	for _, large := range []bool{false, true} {
		files := map[string]string{"notes.md": "updated\n"}
		if large {
			files = map[string]string{"large.go": "package sample\n" + strings.Repeat("x", 256*1024)}
		}
		repo := cliRepo(t, files)
		called := 0
		factory := func(gitdiff.ChangeSet) (llm.Client, error) { called++; return nil, errors.New("must not request") }
		var stdout, stderr bytes.Buffer
		code := runCommand(context.Background(), []string{"--repo", repo, "--format=json"}, &stdout, &stderr, factory)
		var result model.Report
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want, wantCode := model.RunSkipped, 0
		if large {
			want, wantCode = model.RunFailed, 1
		}
		if result.Status != want || code != wantCode || called != 0 || result.Usage.LLMRequests != 0 || *result.Usage.InputTokens != 0 {
			t.Fatalf("skip/size failure confused: %d %+v", code, result)
		}
	}
}

func TestCLIParentCancellationPreservesPartialWork(t *testing.T) {
	repo := cliRepo(t, map[string]string{"a.go": "package sample\n// changed\n", "z.go": "package sample\n// changed\n"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client := &cancelClient{cancel: cancel, calls: &calls}
	factory := func(gitdiff.ChangeSet) (llm.Client, error) { return client, nil }
	var stdout, stderr bytes.Buffer
	code := runCommand(ctx, []string{"--repo", repo, "--format=json"}, &stdout, &stderr, factory)
	var result model.Report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if code != 1 || result.Status != model.RunPartial || calls != 2 || result.Files[1].StopReason != "canceled" {
		t.Fatalf("cancel lost partial state: %d %+v", code, result)
	}
}

type cancelClient struct {
	cancel context.CancelFunc
	calls  *int
}

func (c *cancelClient) Complete(ctx context.Context, request llm.Request) (llm.Response, error) {
	*c.calls++
	if *c.calls == 1 {
		return llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "done_a", Name: "task_done", Arguments: json.RawMessage(`{"summary":"Checked a.go."}`)}}}}, nil
	}
	c.cancel()
	return llm.Response{}, context.Canceled
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("private output detail") }

func TestCLIOutputFailureIsExecutionError(t *testing.T) {
	repo := cliRepo(t, map[string]string{"notes.md": "changed docs\n"})
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	var stderr bytes.Buffer
	if code := runCommand(context.Background(), []string{"--repo", repo, "--mock", "--trace", tracePath}, brokenOutput{}, &stderr, nil); code != 1 || strings.Contains(stderr.String(), "private output detail") {
		t.Fatal("output failure hidden or exposed")
	}
	content, _ := os.ReadFile(tracePath)
	if !strings.Contains(string(content), `"event":"output_error"`) {
		t.Fatal("late report output failure not recorded")
	}
}

func TestCLIMockScriptsRemainIndependentAfterEarlyStop(t *testing.T) {
	repo := cliRepo(t, map[string]string{"a.go": "package sample\n// changed\n", "z.go": "package sample\n// changed\n"})
	var stdout, stderr bytes.Buffer
	code := runCommand(context.Background(), []string{"--repo", repo, "--mock", "--format=json", "--max-rounds=2"}, &stdout, &stderr, nil)
	var result model.Report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if code != 1 || result.Status != model.RunPartial || len(result.Findings) != 2 || result.Findings[0].Path != "a.go" || result.Findings[1].Path != "z.go" || result.Usage.LLMRequests != 4 {
		t.Fatalf("early stop shifted another file script: %d %+v", code, result)
	}
	for _, file := range result.Files {
		if file.StopReason != "max_rounds" || file.Rounds != 2 {
			t.Fatalf("wrong early-stop result: %+v", file)
		}
	}
}

func TestCLIDay6Demo(t *testing.T) {
	directory := os.Getenv("MINI_REVIEW_DEMO_DIR")
	if directory == "" {
		t.Skip("artifact demo requires an explicit fresh MINI_REVIEW_DEMO_DIR")
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	repo := cliRepo(t, map[string]string{"a.go": "package sample\n// changed\n", "z.go": "package sample\n// changed\n"})
	for _, partial := range []bool{false, true} {
		name, format, wantCode := "completed", "json", 0
		var factory clientFactory
		if partial {
			name, format, wantCode = "partial", "markdown", 1
			factory = func(gitdiff.ChangeSet) (llm.Client, error) {
				return llm.NewMockClient([]llm.MockStep{
					{Response: llm.Response{Assistant: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "done_a", Name: "task_done", Arguments: json.RawMessage(`{"summary":"Checked a.go."}`)}}}}},
					{Error: errors.New("scripted model failure")},
				}), nil
			}
		}
		extension := ".json"
		if partial {
			extension = ".md"
		}
		var stdout, stderr bytes.Buffer
		args := []string{"--repo", repo, "--mock", "--format", format, "--output", filepath.Join(directory, name+extension), "--trace", filepath.Join(directory, name+".trace.jsonl")}
		if code := runCommand(context.Background(), args, &stdout, &stderr, factory); code != wantCode || stdout.Len() != 0 {
			t.Fatalf("artifact demo failed: %d %s", code, stderr.String())
		}
		t.Logf("demo=%s report=%s trace=%s exit=%d stdout_empty=true", name, name+extension, name+".trace.jsonl", wantCode)
	}
}
