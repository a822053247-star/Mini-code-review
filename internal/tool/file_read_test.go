// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"example.com/mini-code-review/internal/gitdiff"
	"example.com/mini-code-review/internal/llm"
)

func readTestSnapshot() gitdiff.Snapshot {
	return gitdiff.Snapshot{RepoRoot: "fixture", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40)}
}

func intPointer(value int) *int { return &value }

func memoryFileReader(t *testing.T, content string) *FileReader {
	t.Helper()
	reader, err := NewFileReader(readTestSnapshot(), func(_ context.Context, snapshot gitdiff.Snapshot, path string) ([]byte, error) {
		if snapshot != readTestSnapshot() || path != "sample.go" {
			t.Fatalf("unexpected read input: %+v, %q", snapshot, path)
		}
		return []byte(content), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func readData(t *testing.T, execution Execution, err error) FileReadData {
	t.Helper()
	data, ok := execution.Data.(FileReadData)
	if err != nil || !execution.OK || execution.Error != nil || execution.Completion != nil || !ok {
		t.Fatalf("unexpected read result: %+v, error %v", execution, err)
	}
	if len(data.Content) > maxReadBytes || !utf8.ValidString(data.Content) {
		t.Fatalf("invalid or oversized content: %d bytes", len(data.Content))
	}
	return data
}

func TestSplitSourceLines(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    []string
	}{
		{"", []string{}}, {"a", []string{"a"}}, {"a\n", []string{"a"}},
		{"\n", []string{""}}, {"a\n\n", []string{"a", ""}},
		{" a \r\nb\rtext\r\n", []string{" a ", "b\rtext"}},
	} {
		if got := SplitSourceLines([]byte(tc.content)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("split %q: got %q, want %q", tc.content, got, tc.want)
		}
	}
}

func TestFileReadRangesAndNumbering(t *testing.T) {
	var source strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&source, "// line %d\n", i)
	}
	reader := memoryFileReader(t, source.String())
	for _, tc := range []struct {
		name               string
		start, end         *int
		wantStart, wantEnd int
		truncated          bool
	}{
		{"default", nil, nil, 1, 120, false},
		{"start only", intPointer(10), nil, 10, 129, false},
		{"start near EOF", intPointer(295), nil, 295, 300, false},
		{"end only", nil, intPointer(5), 1, 5, false},
		{"both", intPointer(4), intPointer(5), 4, 5, false},
		{"exact 200 lines", nil, intPointer(200), 1, 200, false},
		{"201 lines", nil, intPointer(201), 1, 200, true},
		{"last line", intPointer(300), intPointer(300), 300, 300, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution, err := reader.Execute(context.Background(), FileReadArgs{Path: "sample.go", StartLine: tc.start, EndLine: tc.end})
			data := readData(t, execution, err)
			var expected strings.Builder
			for i := tc.wantStart; i <= tc.wantEnd; i++ {
				fmt.Fprintf(&expected, "%d|// line %d\n", i, i)
			}
			if data.Path != "sample.go" || data.HeadSHA != readTestSnapshot().HeadSHA || data.TotalLines != 300 ||
				data.StartLine == nil || *data.StartLine != tc.wantStart || data.EndLine == nil || *data.EndLine != tc.wantEnd ||
				data.Content != expected.String() || data.Truncated != tc.truncated || data.LastLineTruncated {
				t.Fatalf("unexpected numbered window: %+v", data)
			}
		})
	}
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct{ start, end *int }{
		{intPointer(0), nil}, {intPointer(-1), nil}, {nil, intPointer(0)}, {nil, intPointer(-1)},
		{intPointer(6), intPointer(5)}, {intPointer(301), nil}, {nil, intPointer(301)},
		{intPointer(maxInt), nil}, {nil, intPointer(maxInt)},
	} {
		execution, err := reader.Execute(context.Background(), FileReadArgs{Path: "sample.go", StartLine: tc.start, EndLine: tc.end})
		if err != nil || execution.OK || execution.Error == nil || execution.Error.Code != "invalid_range" {
			t.Fatalf("illegal range accepted: %+v, error %v", execution, err)
		}
	}
}

func TestFileReadEmptyAndLineEndings(t *testing.T) {
	reader := memoryFileReader(t, "")
	execution, err := reader.Execute(context.Background(), FileReadArgs{Path: "sample.go"})
	data := readData(t, execution, err)
	if data.Content != "" || data.TotalLines != 0 || data.StartLine != nil || data.EndLine != nil || data.Truncated {
		t.Fatalf("unexpected empty file: %+v", data)
	}
	encoded, err := json.Marshal(execution)
	if err != nil || !strings.Contains(string(encoded), `"start_line":null`) || !strings.Contains(string(encoded), `"end_line":null`) {
		t.Fatalf("empty bounds did not serialize as null: %s, error %v", encoded, err)
	}
	for _, args := range []FileReadArgs{
		{Path: "sample.go", StartLine: intPointer(1)}, {Path: "sample.go", EndLine: intPointer(1)},
	} {
		execution, err := reader.Execute(context.Background(), args)
		if err != nil || execution.Error == nil || execution.Error.Code != "invalid_range" {
			t.Fatalf("empty explicit range: %+v, error %v", execution, err)
		}
	}
	for _, source := range []string{" a \nb\rtext", " a \nb\rtext\n", " a \r\nb\rtext\r\n"} {
		execution, err := memoryFileReader(t, source).Execute(context.Background(), FileReadArgs{Path: "sample.go"})
		data := readData(t, execution, err)
		if data.TotalLines != 2 || data.Content != "1| a \n2|b\rtext\n" {
			t.Fatalf("line endings changed content: %+v", data)
		}
	}
}

func TestFileReadByteLimits(t *testing.T) {
	for _, tc := range []struct {
		name, content      string
		wantBytes, wantEnd int
		truncated, partial bool
	}{
		{"exact limit", strings.Repeat("x", maxReadBytes-3), maxReadBytes, 1, false, false},
		{"one byte over", strings.Repeat("x", maxReadBytes-2), maxReadBytes, 1, true, true},
		{"UTF-8 partial line", strings.Repeat("中", maxReadBytes/3+1), 3 + ((maxReadBytes-3)/3)*3, 1, true, true},
		{"stop at full line", "a\n" + strings.Repeat("x", maxReadBytes), 4, 1, true, false},
		{"exact full line then more", strings.Repeat("x", maxReadBytes-3) + "\na\n", maxReadBytes, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution, err := memoryFileReader(t, tc.content).Execute(context.Background(), FileReadArgs{Path: "sample.go"})
			data := readData(t, execution, err)
			if len(data.Content) != tc.wantBytes || *data.EndLine != tc.wantEnd || data.Truncated != tc.truncated || data.LastLineTruncated != tc.partial {
				t.Fatalf("unexpected byte limit result: %+v", data)
			}
		})
	}
}

func TestFileReadRejectsPathsAndExcludedContent(t *testing.T) {
	for _, path := range []string{"", "/a.go", "C:/a.go", "dir\\a.go", "../a.go", "dir/../a.go", "./a.go", "a//b.go", "a\x00.go", "a\n.go", "a\t.go"} {
		reader, err := NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
			t.Fatal("invalid path reached reader")
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		execution, err := reader.Execute(context.Background(), FileReadArgs{Path: path})
		if err != nil || execution.Error == nil || execution.Error.Code != "invalid_path" {
			t.Fatalf("path %q: %+v, error %v", path, execution, err)
		}
	}
	for _, path := range []string{"notes.md", "vendor/a.go", "nested/testdata/a.go"} {
		reader, _ := NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
			t.Fatal("excluded path reached reader")
			return nil, nil
		})
		execution, err := reader.Execute(context.Background(), FileReadArgs{Path: path})
		if err != nil || execution.Error == nil || execution.Error.Code != "file_excluded" {
			t.Fatalf("excluded path %q: %+v, error %v", path, execution, err)
		}
	}
	for _, source := range []string{"// Code generated by fixture. DO NOT EDIT.\npackage sample\n", "// Code generated by fixture. DO NOT EDIT.\r\n", "package sample\x00", "// \xff"} {
		execution, err := memoryFileReader(t, source).Execute(context.Background(), FileReadArgs{Path: "sample.go"})
		if err != nil || execution.Error == nil || execution.Error.Code != "file_excluded" {
			t.Fatalf("excluded content accepted: %+v, error %v", execution, err)
		}
	}
	execution, err := memoryFileReader(t, strings.Repeat("x", gitdiff.MaxHeadFileBytes+1)).Execute(context.Background(), FileReadArgs{Path: "sample.go"})
	if err != nil || execution.Error == nil || execution.Error.Code != "file_too_large" {
		t.Fatalf("oversized injected content: %+v, error %v", execution, err)
	}
}

func TestFileReadErrorsAndCancellation(t *testing.T) {
	if reader, err := NewFileReader(gitdiff.Snapshot{}, nil); err == nil || reader != nil {
		t.Fatal("invalid snapshot accepted")
	}
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{gitdiff.ErrNotRegularFile, "special_file"}, {gitdiff.ErrFileTooLarge, "file_too_large"},
		{errors.New("private diagnostic"), "read_failed"}, {context.Canceled, ""}, {context.DeadlineExceeded, ""},
	} {
		reader, _ := NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
			return nil, fmt.Errorf("read: %w", tc.cause)
		})
		execution, err := reader.Execute(context.Background(), FileReadArgs{Path: "sample.go"})
		if tc.code == "" {
			if !errors.Is(err, tc.cause) || !reflect.DeepEqual(execution, Execution{}) {
				t.Fatalf("lost cancellation cause: %+v, error %v", execution, err)
			}
		} else if err != nil || execution.Error == nil || execution.Error.Code != tc.code {
			t.Fatalf("read error: %+v, error %v", execution, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader, _ := NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
		t.Fatal("canceled read executed")
		return nil, nil
	})
	if _, err := reader.Execute(ctx, FileReadArgs{Path: "sample.go"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader, _ = NewFileReader(readTestSnapshot(), func(context.Context, gitdiff.Snapshot, string) ([]byte, error) {
		cancel()
		return []byte("package sample\n"), nil
	})
	if execution, err := reader.Execute(ctx, FileReadArgs{Path: "sample.go"}); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(execution, Execution{}) {
		t.Fatalf("read success survived cancellation: %+v, error %v", execution, err)
	}
}

// 集成 fixture 只在临时仓库执行写操作，不依赖用户的身份、分支或全局配置。
func fileReadGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeReadFixture(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFileReadFixedHeadThroughRegistry(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	repo := t.TempDir()
	fileReadGit(t, repo, "init", "--template=", "-b", "main")
	for key, value := range map[string]string{
		"user.name": "Mini Review Test", "user.email": "mini-review@example.com", "commit.gpgsign": "false",
		"core.autocrlf": "false", "core.hooksPath": filepath.Join(repo, "no-hooks"),
	} {
		fileReadGit(t, repo, "config", key, value)
	}
	fixtures := map[string]string{
		"main.go": "package sample\n// base\n", "related space.go": "package sample\n// fixed related context\n",
		"myvendor/a.go": "package sample\n", "mytestdata/a.go": "package sample\n", "empty.go": "",
		"vendor/a.go": "package excluded\n", "nested/testdata/a.go": "package excluded\n",
		"generated.go": "// Code generated by fixture. DO NOT EDIT.\npackage sample\n",
		"binary.go":    "package sample\x00\n", "invalid.go": "// \xff\n", "notes.md": "not Go\n",
		"limit.go": strings.Repeat("x", gitdiff.MaxHeadFileBytes), "large.go": strings.Repeat("x", gitdiff.MaxHeadFileBytes+1),
		"dir.go/child.go": "package sample\n",
	}
	for path, content := range fixtures {
		writeReadFixture(t, repo, path, content)
	}
	fileReadGit(t, repo, "add", "-A")
	fileReadGit(t, repo, "commit", "-m", "Base fixture")
	base := fileReadGit(t, repo, "rev-parse", "HEAD")
	writeReadFixture(t, repo, "main.go", "package sample\n// head\n")
	fileReadGit(t, repo, "add", "main.go")
	blob := fileReadGit(t, repo, "rev-parse", base+":main.go")
	fileReadGit(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+blob+",link.go")
	fileReadGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+base+",module.go")
	fileReadGit(t, repo, "commit", "-m", "Head fixture")
	snapshot, err := gitdiff.Resolve(context.Background(), repo, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	set, err := gitdiff.Load(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range set.Files {
		if change.Path == "related space.go" {
			t.Fatal("related fixture unexpectedly changed")
		}
	}
	reader, err := NewFileReader(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	handlers := (&handlerSpy{}).handlers()
	handlers.FileRead = reader.Execute
	registry, err := NewRegistry(handlers)
	if err != nil {
		t.Fatal(err)
	}
	// 移动分支后再留下暂存及未暂存修改，并删除工作区中的关联文件。
	writeReadFixture(t, repo, "main.go", "package later\n")
	fileReadGit(t, repo, "add", "main.go")
	fileReadGit(t, repo, "commit", "-m", "Move branch")
	writeReadFixture(t, repo, "main.go", "package staged\n")
	fileReadGit(t, repo, "add", "main.go")
	writeReadFixture(t, repo, "main.go", "package unstaged\n")
	if err := os.Remove(filepath.Join(repo, "related space.go")); err != nil {
		t.Fatal(err)
	}
	beforeStatus := fileReadGit(t, repo, "status", "--porcelain")
	beforeIndex := fileReadGit(t, repo, "diff", "--cached")
	for _, path := range []string{"main.go", "related space.go", "myvendor/a.go", "mytestdata/a.go", "empty.go", "limit.go"} {
		arguments, _ := json.Marshal(FileReadArgs{Path: path})
		execution, err := registry.Execute(context.Background(), llm.ToolCall{ID: "read_context", Name: FileRead, Arguments: arguments})
		data := readData(t, execution, err)
		if data.HeadSHA != snapshot.HeadSHA {
			t.Fatal("head SHA drifted")
		}
		if path == "main.go" && data.Content != "1|package sample\n2|// head\n" {
			t.Fatalf("read followed moving branch or worktree: %+v", data)
		}
		if path == "related space.go" && data.Content != "1|package sample\n2|// fixed related context\n" {
			t.Fatalf("unchanged related content drifted: %+v", data)
		}
		if path == "limit.go" && (!data.Truncated || !data.LastLineTruncated) {
			t.Fatal("large legal file did not truncate its first line")
		}
	}
	for path, code := range map[string]string{
		"vendor/a.go": "file_excluded", "nested/testdata/a.go": "file_excluded", "notes.md": "file_excluded",
		"generated.go": "file_excluded", "binary.go": "file_excluded", "invalid.go": "file_excluded",
		"large.go": "file_too_large", "link.go": "special_file", "module.go": "special_file",
		"dir.go": "special_file", "missing.go": "read_failed",
	} {
		arguments, _ := json.Marshal(FileReadArgs{Path: path})
		execution, err := registry.Execute(context.Background(), llm.ToolCall{Name: FileRead, Arguments: arguments})
		if err != nil || execution.Error == nil || execution.Error.Code != code {
			t.Fatalf("read %q: %+v, error %v, want %s", path, execution, err, code)
		}
	}
	if fileReadGit(t, repo, "status", "--porcelain") != beforeStatus || fileReadGit(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("file_read modified worktree or index")
	}
}
