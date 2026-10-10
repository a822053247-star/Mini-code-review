// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 测试从仓库子目录调用 Resolve，能否正确找到仓库根目录并固定提交版本。
func TestResolvePinsCommitsFromSubdirectory(t *testing.T) {
	repo := newRepo(t)
	base := commitFile(t, repo, "package sample\n")
	head := commitFile(t, repo, "package sample\n// second version\n")
	t.Logf("base: %s, head: %s", base, head)
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
	// Keep fixtures independent of the user's Git configuration and templates.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	// 假设临时路径是 C:\Temp\Test001
	repo := t.TempDir()                                   //repo == "C:\\Temp\\Test001"
	gitTest(t, repo, "init", "--template=", "-b", "main") // 在repo目录下初始化一个 Git 仓库，.git目录下包含HEAD、refs目录、objects目录等文件
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

func TestGetChangesAndReadFixedHead(t *testing.T) {
	repo := newRepo(t)
	for name, content := range map[string]string{
		"space name.go": "package sample\n// old space\n",
		"deleted.go":    "package sample\n",
		"old.go":        "package renamed\n",
		"literal[1].go": "package sample\n// old literal\n",
		"literal1.go":   "package sample\n// old neighbor\n",
		"type.go":       "package sample\n",
	} {
		writeNamedFile(t, repo, name, content)
	}
	base := commitAll(t, repo)
	writeNamedFile(t, repo, "space name.go", "package sample\n// new space\n")
	writeNamedFile(t, repo, "literal[1].go", "package sample\n// new literal\n")
	writeNamedFile(t, repo, "literal1.go", "package sample\n// neighbor must not leak\n")
	writeNamedFile(t, repo, "added.go", "package added\n")
	gitTest(t, repo, "rm", "deleted.go")
	gitTest(t, repo, "mv", "old.go", "renamed.go")
	gitTest(t, repo, "add", "-A")
	// Construct special entries in the index without Windows symlink privileges.
	linkBlob := gitTest(t, repo, "rev-parse", base+":type.go")
	gitTest(t, repo, "update-index", "--cacheinfo", "120000,"+linkBlob+",type.go")
	gitTest(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+base+",module")
	gitTest(t, repo, "commit", "-m", "Add changes and special entries")
	snapshot, err := Resolve(context.Background(), repo, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := GetChanges(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Path: "added.go", Type: ChangeAdded},
		{Path: "deleted.go", Type: ChangeDeleted},
		{Path: "literal1.go", Type: ChangeModified},
		{Path: "literal[1].go", Type: ChangeModified},
		{Path: "module", Type: ChangeAdded},
		{Path: "old.go", Type: ChangeDeleted},
		{Path: "renamed.go", Type: ChangeAdded},
		{Path: "space name.go", Type: ChangeModified},
		{Path: "type.go", Type: ChangeTypeChanged},
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %d entries", changes, len(want))
	}
	for i, change := range changes {
		if change.Path != want[i].Path || change.Type != want[i].Type || change.RawPatch == "" {
			t.Fatalf("change %d = %+v, want %+v with patch", i, change, want[i])
		}
		if change.Path == "literal[1].go" && (strings.Contains(change.RawPatch, "neighbor") || strings.Count(change.RawPatch, "diff --git ") != 1) {
			t.Fatalf("literal path expanded: %s", change.RawPatch)
		}
	}
	writeNamedFile(t, repo, "added.go", "uncommitted replacement\n")
	if err := os.Remove(filepath.Join(repo, "space name.go")); err != nil {
		t.Fatal(err)
	}
	// Move HEAD after resolution; neither patches nor file content may drift.
	commitFile(t, repo, "package sample\n// later commit\n")
	beforeStatus := gitTest(t, repo, "status", "--porcelain")
	beforeIndex := gitTest(t, repo, "diff", "--cached")
	again, err := GetChanges(context.Background(), snapshot)
	if err != nil || !reflect.DeepEqual(again, changes) {
		t.Fatalf("fixed changes drifted: %v", err)
	}
	for name, wantContent := range map[string]string{
		"added.go":      "package added\n",
		"space name.go": "package sample\n// new space\n",
		"literal[1].go": "package sample\n// new literal\n",
	} {
		content, err := ReadHeadFile(context.Background(), snapshot, name)
		if err != nil || string(content) != wantContent {
			t.Fatalf("read %q = %q, error %v", name, content, err)
		}
	}
	for _, name := range []string{"type.go", "module"} {
		if _, err := ReadHeadFile(context.Background(), snapshot, name); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("special entry %q was accepted: %v", name, err)
		}
	}
	if gitTest(t, repo, "status", "--porcelain") != beforeStatus || gitTest(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("reading changes or head files modified the worktree or index")
	}
}

func TestReadHeadFileValidationAndSize(t *testing.T) {
	repo := newRepo(t)
	writeNamedFile(t, repo, "dir/file.go", "package sample\n")
	writeNamedFile(t, repo, "empty.go", "")
	writeNamedFile(t, repo, "limit.go", strings.Repeat("x", maxHeadFileBytes))
	writeNamedFile(t, repo, "large.go", strings.Repeat("x", maxHeadFileBytes+1))
	head := commitAll(t, repo)
	snapshot, err := Resolve(context.Background(), repo, head, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "/file.go", "C:/file.go", "../file.go", "dir/../file.go", "dir\\file.go", "dir//file.go", "./file.go", "file\n.go", "file\x00.go", "missing.go", "dir"} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadHeadFile(context.Background(), snapshot, name); err == nil {
				t.Fatalf("invalid path %q was accepted", name)
			}
		})
	}
	if content, err := ReadHeadFile(context.Background(), snapshot, "limit.go"); err != nil || len(content) != maxHeadFileBytes {
		t.Fatalf("size boundary: %d bytes, error %v", len(content), err)
	}
	if _, err := ReadHeadFile(context.Background(), snapshot, "large.go"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversized file: %v", err)
	}
	if content, err := ReadHeadFile(context.Background(), snapshot, "empty.go"); err != nil || len(content) != 0 {
		t.Fatalf("empty file: %q, error %v", content, err)
	}
	if content, err := ReadHeadFile(context.Background(), snapshot, "dir/file.go"); err != nil || string(content) != "package sample\n" {
		t.Fatalf("nested file: %q, error %v", content, err)
	}
	changes, err := GetChanges(context.Background(), snapshot)
	if err != nil || changes == nil || len(changes) != 0 {
		t.Fatalf("identical commits: %+v, error %v", changes, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadHeadFile(ctx, snapshot, "empty.go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation: %v", err)
	}
	if _, err := GetChanges(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("changes cancellation: %v", err)
	}
}

func TestParseChangedPaths(t *testing.T) {
	got, err := parseChangedPaths([]byte("M\x00z space.go\x00A\x00a\tname.go\x00"))
	want := []FileChange{{Path: "a\tname.go", Type: ChangeAdded}, {Path: "z space.go", Type: ChangeModified}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("NUL parsing = %+v, error %v", got, err)
	}
	for _, raw := range []string{"M\x00file.go", "M\x00", "R100\x00old\x00new\x00", "A\x00\x00"} {
		if _, err := parseChangedPaths([]byte(raw)); err == nil {
			t.Fatalf("malformed list accepted: %q", raw)
		}
	}
	if _, err := GetChanges(context.Background(), Snapshot{RepoRoot: "repo", BaseSHA: "main", HeadSHA: "HEAD"}); err == nil {
		t.Fatal("mutable refs were accepted as snapshot object IDs")
	}
}

func TestGetChangesDisablesDiffDrivers(t *testing.T) {
	repo := newRepo(t)
	writeNamedFile(t, repo, ".gitattributes", "sample.go diff=custom\n")
	base := commitFile(t, repo, "package sample\n")
	gitTest(t, repo, "add", ".gitattributes")
	gitTest(t, repo, "commit", "-m", "Add diff attributes")
	head := commitFile(t, repo, "package sample\n// changed\n")
	gitTest(t, repo, "config", "diff.custom.command", "missing-external-diff-command")
	gitTest(t, repo, "config", "diff.custom.textconv", "missing-textconv-command")
	gitTest(t, repo, "config", "diff.renames", "true")
	gitTest(t, repo, "config", "diff.noprefix", "true")
	gitTest(t, repo, "config", "color.ui", "always")
	snapshot, err := Resolve(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := GetChanges(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Path == "sample.go" {
			if !strings.Contains(change.RawPatch, "--- a/sample.go\n+++ b/sample.go") || !strings.Contains(change.RawPatch, "+// changed") || strings.Contains(change.RawPatch, "\x1b") {
				t.Fatalf("patch was affected by diff configuration: %q", change.RawPatch)
			}
			return
		}
	}
	t.Fatal("sample.go patch missing")
}

func writeNamedFile(t *testing.T, repo, name, content string) {
	t.Helper()
	filePath := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, repo string) string {
	t.Helper()
	gitTest(t, repo, "add", "-A")
	gitTest(t, repo, "commit", "-m", "Update fixture files")
	return gitTest(t, repo, "rev-parse", "HEAD")
}
