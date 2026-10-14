// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
	"example.com/mini-code-review/internal/model"
	"example.com/mini-code-review/internal/tool"
)

// TestAgentLiveSmoke is explicitly opt-in; regular go test never uses a key or
// sends external requests, even when model credentials are configured.
func TestAgentLiveSmoke(t *testing.T) {
	if os.Getenv("MINI_REVIEW_LIVE") != "1" {
		t.Skip("live model smoke requires explicit MINI_REVIEW_LIVE=1")
	}
	config, err := llm.HTTPConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	client, err := llm.NewHTTPClient(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	repo := t.TempDir()
	offlineDemoGit(t, repo, "init", "--template=", "-b", "main")
	for key, value := range map[string]string{"user.name": "Mini Review Test", "user.email": "mini-review@example.com", "commit.gpgsign": "false", "core.autocrlf": "false", "core.hooksPath": filepath.Join(repo, "no-hooks")} {
		offlineDemoGit(t, repo, "config", key, value)
	}
	const related = "package demo\n\nimport \"errors\"\n\ntype User struct { Name string }\n\nfunc Lookup(id int) (*User, error) {\n\tif id < 1 { return nil, errors.New(\"invalid user ID\") }\n\treturn &User{Name: \"Ada\"}, nil\n}\n"
	const base = "package demo\n\nfunc Name(id int) (string, error) {\n\tuser, err := Lookup(id)\n\tif err != nil { return \"\", err }\n\treturn user.Name, nil\n}\n"
	const head = "package demo\n\nfunc Name(id int) (string, error) {\n\tuser, _ := Lookup(id)\n\treturn user.Name, nil\n}\n"
	offlineDemoWrite(t, repo, "service.go", base)
	offlineDemoWrite(t, repo, "related.go", related)
	offlineDemoGit(t, repo, "add", "-A")
	offlineDemoGit(t, repo, "commit", "-m", "Base live smoke fixture")
	offlineDemoWrite(t, repo, "service.go", head)
	offlineDemoGit(t, repo, "add", "service.go")
	offlineDemoGit(t, repo, "commit", "-m", "Head live smoke fixture")
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 3*time.Minute)
	defer cancel()
	snapshot, err := gitdiff.Resolve(ctx, repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	offlineDemoWrite(t, repo, "service.go", "package staged\n")
	offlineDemoGit(t, repo, "add", "service.go")
	offlineDemoWrite(t, repo, "service.go", "package unstaged\n")
	beforeStatus, beforeIndex := offlineDemoGit(t, repo, "status", "--porcelain"), offlineDemoGit(t, repo, "diff", "--cached")
	set, err := gitdiff.Load(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	requests, reads, comments, done := 0, 0, 0, 0
	observed := testClient(func(ctx context.Context, request llm.Request) (llm.Response, error) {
		requests++
		response, err := client.Complete(ctx, request)
		if err != nil {
			t.Logf("request=%d model_request_failed=true", requests)
			return response, err
		}
		t.Logf("request=%d assistant_tool_calls=%d", requests, len(response.Assistant.ToolCalls))
		for _, call := range response.Assistant.ToolCalls {
			switch call.Name {
			case tool.FileRead:
				reads++
			case tool.CodeComment:
				comments++
			case tool.TaskDone:
				done++
			default:
				t.Logf("request=%d tool=unregistered", requests)
				continue
			}
			t.Logf("request=%d tool=%s", requests, call.Name)
		}
		return response, nil
	})
	agent, err := NewAgent(observed, RunnerOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(ctx, set, "Review the changed Name function. The Lookup contract is in unchanged related.go; inspect both files before deciding whether the change is safe.")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range result.Files {
		t.Logf("file=%s status=%s stop_reason=%s rounds=%d", file.Path, file.Status, file.StopReason, file.Rounds)
	}
	encodedUsage, err := json.Marshal(result.Usage)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s base_sha=%s head_sha=%s findings=%d reads=%d comments=%d done_calls=%d usage=%s", config.Model, snapshot.BaseSHA, snapshot.HeadSHA, len(result.Findings), reads, comments, done, encodedUsage)
	if result.HasCoverageGap || len(result.FileErrors) != 0 {
		for _, failure := range result.FileErrors {
			t.Logf("file_error=%v", failure)
		}
		t.Fatal("live review did not complete")
	}
	if len(result.Files) != 1 || result.Files[0].Status != model.FileCompleted || reads == 0 || done == 0 {
		t.Fatal("live model did not complete the tool-driven file review")
	}
	for _, finding := range result.Findings {
		if finding.Path != "service.go" || finding.StartLine != 4 {
			t.Fatal("finding was not anchored to the loaded added line")
		}
		t.Logf("finding_path=%s start_line=%d end_line=%d severity=%s category=%s", finding.Path, finding.StartLine, finding.EndLine, finding.Severity, finding.Category)
	}
	if offlineDemoGit(t, repo, "status", "--porcelain") != beforeStatus || offlineDemoGit(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("live review modified fixture worktree or index")
	}
	if strings.TrimSpace(offlineDemoGit(t, repo, "rev-list", "--count", "HEAD")) != "2" {
		t.Fatal("live fixture must contain exactly two commits")
	}
	t.Log("worktree_and_index_unchanged=true model_reasoning_logged=false credentials_persisted=false")
}
