// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	gitTimeout       = 10 * time.Second
	maxStderrBytes   = 4096
	maxHeadFileBytes = 256 * 1024
)

// ErrFileTooLarge distinguishes a size limit from a missing or invalid file.
var ErrFileTooLarge = errors.New("head file exceeds 256 KiB")

// ErrNotRegularFile identifies symlinks, trees, and submodules.
var ErrNotRegularFile = errors.New("head path is not a regular file")

// GetChanges returns all changed paths, sorted, without review filtering.
func GetChanges(ctx context.Context, snapshot Snapshot) ([]FileChange, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}

	// git diff --name-status -z --no-renames --no-ext-diff --no-textconv --no-color --ignore-submodules=none BaseSHA HeadSHA --
	// 在指定 Git 仓库，对比 `BaseSHA`（基准提交）和 `HeadSHA`（目标提交），输出变更文件清单，不识别重命名，用 NUL (\0) 字符分隔路径**。
	// `--name-status`：输出变更类型（A 新增 / M 修改 / D 删除）+ 文件路径
	// 例如：Output为：M␀hello.txt␀、D␀readme.md␀、A␀new.md␀ nul是分隔符

	output, err := runGit(ctx, snapshot.RepoRoot, "list changed files",
		"diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
		"--ignore-submodules=none", snapshot.BaseSHA, snapshot.HeadSHA, "--")
	if err != nil {
		return nil, err
	}
	changes, err := parseChangedPaths(output)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		// --literal-pathspecs prevents wildcard and pathspec-magic expansion.
		// git diff --patch --unified=3 --no-renames --no-ext-diff --no-textconv --no-color --src-prefix=a/ --dst-prefix=b/ --ignore-submodules=none BaseSHA HeadSHA -- path/to/file
		// 生成每个变更文件的完整补丁，包含上下文行，便于后续分析。
		// 例如-Hello World  +Hello Git 代表删除了 "Hello World" 并新增了 "Hello Git"
		patch, err := runGit(ctx, snapshot.RepoRoot, "read file patch",
			"--literal-pathspecs", "diff", "--patch", "--unified=3", "--no-renames",
			"--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--ignore-submodules=none",
			snapshot.BaseSHA, snapshot.HeadSHA, "--", changes[i].Path)
		if err != nil {
			return nil, err
		}
		changes[i].RawPatch = string(patch)
	}
	return changes, nil
}

// parseChangedPaths 解析 git diff 输出的变更文件列表，返回按路径排序的 FileChange 切片。
func parseChangedPaths(output []byte) ([]FileChange, error) {
	changes := []FileChange{}
	if len(output) == 0 {
		return changes, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("changed file list is not NUL-terminated")
	}
	parts := bytes.Split(output[:len(output)-1], []byte{0})
	if len(parts)%2 != 0 {
		return nil, fmt.Errorf("changed file list has incomplete status/path pairs")
	}
	for i := 0; i < len(parts); i += 2 {
		kind := ChangeType(parts[i])
		switch kind {
		case ChangeAdded, ChangeModified, ChangeDeleted, ChangeTypeChanged:
		default:
			return nil, fmt.Errorf("unsupported change status %q", kind)
		}
		if len(parts[i+1]) == 0 {
			return nil, fmt.Errorf("changed file path is empty")
		}
		changes = append(changes, FileChange{Path: string(parts[i+1]), Type: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// ReadHeadFile reads a regular blob from the fixed head, never the worktree.
func ReadHeadFile(ctx context.Context, snapshot Snapshot, filePath string) ([]byte, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}
	if err := validateFilePath(filePath); err != nil {
		return nil, err
	}
	// -z preserves the exact path; -l includes blob size before reading content.
	output, err := runGit(ctx, snapshot.RepoRoot, "inspect head file",
		"--literal-pathspecs", "ls-tree", "--full-tree", "-z", "-l", snapshot.HeadSHA, "--", filePath)
	if err != nil {
		return nil, err
	}
	header, listedPath, found := strings.Cut(string(output), "\t")
	if !found || listedPath != filePath+"\x00" {
		return nil, fmt.Errorf("head file %q not found", filePath)
	}
	fields := strings.Fields(header)
	if len(fields) != 4 {
		return nil, fmt.Errorf("invalid head tree entry for %q", filePath)
	}
	if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, fmt.Errorf("head path %q is not a regular file: %w", filePath, ErrNotRegularFile)
	}
	size, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("invalid head file size for %q", filePath)
	}
	if size > maxHeadFileBytes {
		return nil, fmt.Errorf("head file %q: %w", filePath, ErrFileTooLarge)
	}
	// Blob IDs are immutable; read the inspected object instead of a mutable path.
	content, err := runGit(ctx, snapshot.RepoRoot, "read head file", "cat-file", "blob", fields[2])
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != size {
		return nil, fmt.Errorf("head file %q content size does not match tree entry", filePath)
	}
	return content, nil
}

func validateSnapshot(snapshot Snapshot) error {
	if strings.TrimSpace(snapshot.RepoRoot) == "" {
		return fmt.Errorf("snapshot repository root is empty")
	}
	for _, sha := range []string{snapshot.BaseSHA, snapshot.HeadSHA} {
		if len(sha) != 40 && len(sha) != 64 {
			return fmt.Errorf("snapshot requires full commit object IDs")
		}
		if _, err := hex.DecodeString(sha); err != nil {
			return fmt.Errorf("invalid snapshot commit object ID: %w", err)
		}
	}
	return nil
}

func validateFilePath(filePath string) error {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.ContainsAny(filePath, "\\:") {
		return fmt.Errorf("file path must be a repository-relative path using '/' separators")
	}
	for _, r := range filePath {
		if unicode.IsControl(r) {
			return fmt.Errorf("file path must not contain control characters")
		}
	}
	for _, part := range strings.Split(filePath, "/") {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("file path must not contain traversal or empty components")
		}
	}
	return nil
}

// Resolve 查找工作区根目录，并分别将 base、head 解析一次，保存为固定提交 ID。
// 后续读取使用返回的 SHA，避免分支移动后读取到另一个版本；两端解析并非原子操作。
func Resolve(ctx context.Context, repo, base, head string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("resolve review input: %w", err)
	}
	// 拒绝空白、选项前缀和 NUL，避免非法引用进入 Git 命令。
	for _, ref := range []struct{ name, value string }{{"base", base}, {"head", head}} {
		if strings.TrimSpace(ref.value) == "" || strings.HasPrefix(ref.value, "-") || strings.ContainsRune(ref.value, '\x00') {
			return Snapshot{}, fmt.Errorf("invalid %s ref: must not be blank, start with '-', or contain NUL", ref.name)
		}
	}
	if strings.TrimSpace(repo) == "" {
		return Snapshot{}, fmt.Errorf("repository path must not be blank")
	}
	root, err := filepath.Abs(repo)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect repository directory: %w", err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("repository path must be a directory")
	}
	// git rev-parse --is-inside-work-tree 命令可以检查当前目录是否是一个 Git 工作树
	inside, err := runGit(ctx, root, "check Git worktree", "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return Snapshot{}, fmt.Errorf("repository must be a non-bare Git worktree")
	}
	// git rev-parse --show-toplevel 命令可以获取当前 Git 仓库的根目录
	output, err := runGit(ctx, root, "resolve repository root", "rev-parse", "--show-toplevel")
	if err != nil {
		return Snapshot{}, err
	}
	// 只去除末尾换行，保留路径名中的普通空格。
	root = filepath.Clean(strings.TrimRight(string(output), "\r\n"))
	baseSHA, err := resolveCommit(ctx, root, "base", base)
	if err != nil {
		if base == "HEAD~1" {
			return Snapshot{}, fmt.Errorf("default base HEAD~1 could not be resolved; provide an explicit valid base commit: %w", err)
		}
		return Snapshot{}, err
	}
	headSHA, err := resolveCommit(ctx, root, "head", head)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{RepoRoot: root, BaseSHA: baseSHA, HeadSHA: headSHA}, nil
}

// resolveCommit 验证引用可解析为提交，并检查 Git 返回的对象 ID 格式。
func resolveCommit(ctx context.Context, repo, name, ref string) (string, error) {
	// --end-of-options 结束选项解析；^{commit} 要求引用最终指向提交。
	// 输出完整的commit hash
	output, err := runGit(ctx, repo, "resolve "+name+" commit",
		"rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(output))
	if len(sha) != 40 && len(sha) != 64 {
		return "", fmt.Errorf("resolve %s commit: Git returned an invalid object ID", name)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return "", fmt.Errorf("resolve %s commit: invalid object ID: %w", name, err)
	}
	return sha, nil
}

// runGit 用参数数组调用 Git，为每次调用设置超时并限制 stderr 捕获长度。
func runGit(ctx context.Context, repo, operation string, args ...string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	// 此处只创建命令对象，下面的 Run 才启动并等待进程。
	cmd := exec.CommandContext(requestCtx, "git", args...)
	cmd.Dir = repo
	// 限制取消或进程退出后的额外等待，不替代正常执行的 10 秒超时。
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	stderr := cappedBuffer{limit: maxStderrBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// 保留可用 errors.Is 识别的取消或超时原因。
		if contextErr := requestCtx.Err(); contextErr != nil {
			err = contextErr
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %w; Git stderr: %s", operation, err, detail)
		}
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return stdout.Bytes(), nil
}

// cappedBuffer 接受所有输入，但只保存前 limit 字节，避免 stderr 无限增长。
// buffer 使用具名字段，避免嵌入 bytes.Buffer 的 ReadFrom 绕过大小限制。
type cappedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

// Write 实现 io.Writer；超出的内容会被消费并丢弃，而不是触发写入错误。
func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		b.buffer.Write(p[:min(len(p), remaining)])
	}
	// 丢弃的字节也算已消费，避免 io.Copy 将截断误判为短写错误。
	return len(p), nil
}

// String 返回保留下来的诊断文本，并在发生截断时附加提示。
func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buffer.String() + " [truncated]"
	}
	return b.buffer.String()
}
