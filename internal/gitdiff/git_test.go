// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 测试从仓库子目录调用 Resolve，能否正确找到仓库根目录并固定提交版本。
func TestResolvePinsCommitsFromSubdirectory(t *testing.T) {
	repo := newRepo(t)
	base := commitFile(t, repo, "package sample\n")
	head := commitFile(t, repo, "package sample\n// second version\n")
	fmt.Printf("base: %s, head: %s\n", base, head)
	subdir := filepath.Join(repo, "sub directory")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	// stage在git中是暂存区的意思
	// Leave both staged and unstaged changes to verify resolution is read-only.
	writeFile(t, repo, "package sample\n// staged version\n")
	gitTest(t, repo, "add", "sample.go")
	writeFile(t, repo, "package sample\n// unstaged version\n")
	// git status --porcelain 命令可以查看当前工作区的状态，输出格式为简洁模式
	beforeStatus := gitTest(t, repo, "status", "--porcelain")
	// git diff --cached 命令可以查看暂存区和工作区的差异
	beforeIndex := gitTest(t, repo, "diff", "--cached")
	// HEAD~1 代表当前提交（HEAD）的上一个提交，也就是父提交
	snapshot, err := Resolve(context.Background(), subdir, "HEAD~1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != base || snapshot.HeadSHA != head {
		t.Fatalf("snapshot = %+v, want base %s and head %s", snapshot, base, head)
	}
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(snapshot.RepoRoot) != filepath.Clean(wantRoot) {
		t.Fatalf("root = %q, want %q", snapshot.RepoRoot, wantRoot)
	}
	if gitTest(t, repo, "status", "--porcelain") != beforeStatus || gitTest(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("Resolve modified the worktree or index")
	}
	third := commitFile(t, repo, "package sample\n// third version\n")
	if third == snapshot.HeadSHA || snapshot.HeadSHA != head {
		t.Fatal("snapshot did not retain the original head after the branch moved")
	}
	if got := gitTest(t, repo, "show", snapshot.HeadSHA+":sample.go"); got != "package sample\n// second version" {
		t.Fatalf("fixed head content changed: %q", got)
	}
}

func TestResolveInvalidInput(t *testing.T) {
	repo := newRepo(t)
	commitFile(t, repo, "package sample\n")
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitTest(t, repo, "init", "--bare", bare)
	file := filepath.Join(repo, "sample.go")
	for _, tc := range []struct {
		name, repo, base, head, message string
	}{
		{"blank repo", " ", "HEAD", "HEAD", "repository path"},
		{"missing directory", filepath.Join(repo, "missing"), "HEAD", "HEAD", "inspect repository"},
		{"file path", file, "HEAD", "HEAD", "must be a directory"},
		{"not a repository", t.TempDir(), "HEAD", "HEAD", "check Git worktree"},
		{"bare repository", bare, "HEAD", "HEAD", "non-bare"},
		{"blank base", repo, " ", "HEAD", "invalid base ref"},
		{"blank head", repo, "HEAD", "", "invalid head ref"},
		{"option-like base", repo, "--all", "HEAD", "invalid base ref"},
		{"option-like head", repo, "HEAD", "-main", "invalid head ref"},
		{"NUL ref", repo, "HEAD", "HEAD\x00", "invalid head ref"},
		{"missing base", repo, "missing-ref", "HEAD", "resolve base commit"},
		{"missing head", repo, "HEAD", "missing-ref", "resolve head commit"},
		{"non-commit object", repo, "HEAD", "HEAD:sample.go", "resolve head commit"},
		{"root commit default base", repo, "HEAD~1", "HEAD", "explicit valid base commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := Resolve(context.Background(), tc.repo, tc.base, tc.head)
			if err == nil || !strings.Contains(err.Error(), tc.message) || snapshot != (Snapshot{}) {
				t.Fatalf("snapshot = %+v, error = %v; want %q", snapshot, err, tc.message)
			}
		})
	}
}

func TestGitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Resolve(ctx, t.TempDir(), "HEAD", "HEAD"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve cancellation: %v", err)
	}
	if _, err := runGit(ctx, t.TempDir(), "cancelled operation", "--version"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Git cancellation: %v", err)
	}
	deadlineCtx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := runGit(deadlineCtx, t.TempDir(), "expired operation", "--version"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Git deadline: %v", err)
	}
}

func TestGitErrorsAndStderrLimit(t *testing.T) {
	_, err := runGit(context.Background(), t.TempDir(), "test failed command", "not-a-git-command")
	if err == nil || !strings.Contains(err.Error(), "test failed command") || !strings.Contains(err.Error(), "Git stderr:") {
		t.Fatalf("missing operation or stderr: %v", err)
	}
	buffer := cappedBuffer{limit: maxStderrBytes}
	input := []byte(strings.Repeat("x", maxStderrBytes+100))
	n, err := io.Copy(&buffer, struct{ io.Reader }{strings.NewReader(string(input))})
	if err != nil || n != int64(len(input)) || buffer.buffer.Len() != maxStderrBytes || !strings.HasSuffix(buffer.String(), " [truncated]") {
		t.Fatalf("stderr was not bounded: n=%d, bytes=%d, error=%v", n, buffer.buffer.Len(), err)
	}
}

func TestResolveWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve(context.Background(), t.TempDir(), "HEAD", "HEAD")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing Git: %v", err)
	}
}

// 返回一个保存目录路径的字符串，用于存储 Git 仓库的根目录
func newRepo(t *testing.T) string {
	t.Helper()
	// 假设临时路径是 C:\Temp\Test001
	repo := t.TempDir()                    //repo == "C:\\Temp\\Test001"
	gitTest(t, repo, "init", "-b", "main") // 在repo目录下初始化一个 Git 仓库，.git目录下包含HEAD、refs目录、objects目录等文件
	gitTest(t, repo, "config", "user.name", "Mini Review Test")
	gitTest(t, repo, "config", "user.email", "mini-review@example.com")
	gitTest(t, repo, "config", "commit.gpgsign", "false")
	gitTest(t, repo, "config", "core.autocrlf", "false")
	gitTest(t, repo, "config", "core.hooksPath", filepath.Join(repo, "no-hooks"))
	return repo
}

func writeFile(t *testing.T, repo, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "sample.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, repo, content string) string {
	t.Helper()
	writeFile(t, repo, content)
	gitTest(t, repo, "add", "sample.go")
	gitTest(t, repo, "commit", "-m", "Update sample")

	// git rev-parse HEAD 命令 可以拿到当前分支最新提交的完整 commit 哈希值
	return gitTest(t, repo, "rev-parse", "HEAD")
}

// 测试辅助函数，在指定目录执行 Git 命令，并返回输出文本
func gitTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput() //CombinedOutput负责真正启动命令，返回输出和错误
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
