// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	gitTimeout     = 10 * time.Second // 每次 Git 调用最多执行 10 秒；上游更早截止时服从上游。
	maxStderrBytes = 4096             // 标准错误正文最多保留前 4096 字节，不包含附加的截断提示。
)

// Resolve 查找工作区根目录，并分别将 base、head 解析一次，保存为固定提交 ID。
// 后续读取使用返回的 SHA，避免分支移动后读取到另一个版本；两端解析并非原子操作。
// 例如main分支名字不变，如果有新的提交进来，虽然还是叫main分支，但是SHA哈希变了。
// 所以这里用SHA哈希锁定版本，避免冲突
func Resolve(ctx context.Context, repo, base, head string) (Snapshot, error) {
	if err := ctx.Err(); err != nil { // 如果上游已经取消或超时，就不再查询文件系统或启动 Git。
		return Snapshot{}, fmt.Errorf("resolve review input: %w", err)
	}
	for _, ref := range []struct{ name, value string }{{"base", base}, {"head", head}} {
		// 例子：{{ strings.TrimSpace "\n\r\t   foo   \n\r\t" }} → foo
		// `ref.name="base"`, `ref.value=base`
		if strings.TrimSpace(ref.value) == "" || strings.HasPrefix(ref.value, "-") || strings.ContainsRune(ref.value, '\x00') {
			return Snapshot{}, fmt.Errorf("invalid %s ref: must not be blank, start with '-', or contain NUL", ref.name)
		}
	}
	if strings.TrimSpace(repo) == "" { // 空白路径不应被隐式当成当前目录。
		return Snapshot{}, fmt.Errorf("repository path must not be blank") // 返回明确的路径输入错误。
	} // 路径非空检查结束。
	root, err := filepath.Abs(repo) // 将相对路径转为绝对路径，此时还没有确认仓库根目录。
	if err != nil {                 // 处理绝对路径转换失败。
		return Snapshot{}, fmt.Errorf("resolve repository path: %w", err) // 保留原始路径错误。
	} // 路径转换错误处理结束。
	info, err := os.Stat(root) // 查询路径元数据；路径不存在时会返回错误。
	if err != nil {            // 处理不存在、无权限等文件系统错误。
		return Snapshot{}, fmt.Errorf("inspect repository directory: %w", err) // 给底层错误补充操作背景。
	} // 元数据读取错误处理结束。
	if !info.IsDir() { // 输入必须指向目录，而不是文件。
		return Snapshot{}, fmt.Errorf("repository path must be a directory") // 拒绝文件路径。
	}
	inside, err := runGit(ctx, root, "check Git worktree", "rev-parse", "--is-inside-work-tree") // 查询当前位置是否在 Git 工作区中；允许传入仓库子目录。
	if err != nil {                                                                              // 非仓库、Git 不可用或执行失败都会进入此分支。
		return Snapshot{}, err // runGit 已补充操作名称，直接向上传递。
	} // Git 工作区查询错误处理结束。
	if strings.TrimSpace(string(inside)) != "true" { // 裸仓库和 .git 内部目录不属于工作区，不满足当前项目要求。
		return Snapshot{}, fmt.Errorf("repository must be a non-bare Git worktree") // 明确要求非 bare 的工作仓库。
	} // 工作区结果检查结束。
	output, err := runGit(ctx, root, "resolve repository root", "rev-parse", "--show-toplevel") // 让 Git 返回工作区根目录。
	if err != nil {                                                                             // 处理根目录查询失败。
		return Snapshot{}, err // 不继续使用未经确认的输入目录。
	} // 根目录查询错误处理结束。
	root = filepath.Clean(strings.TrimRight(string(output), "\r\n")) // 只去除输出末尾的换行，再整理路径格式，保留目录名中的普通空格。
	baseSHA, err := resolveCommit(ctx, root, "base", base)           // 将基线引用解析为固定的 commit 对象 ID。
	if err != nil {                                                  // 基线必须有效，否则无法形成比较范围。
		if base == "HEAD~1" { // 默认基线在根提交等情况下可能不存在。
			return Snapshot{}, fmt.Errorf("default base HEAD~1 could not be resolved; provide an explicit valid base commit: %w", err) // 提示显式指定基线，不猜测替代版本。
		} // 默认基线提示处理结束。
		return Snapshot{}, err // 非默认基线失败时保留原始解析错误。
	} // 基线解析错误处理结束。
	headSHA, err := resolveCommit(ctx, root, "head", head) // 将目标引用解析为固定 commit ID，不保存易变的分支名。
	if err != nil {                                        // 目标版本同样必须有效。
		return Snapshot{}, err // 不返回仅包含 base 的不完整快照。
	} // 目标解析错误处理结束。
	return Snapshot{RepoRoot: root, BaseSHA: baseSHA, HeadSHA: headSHA}, nil // 成功返回根目录与两个固定版本。
} // Resolve 结束。

// resolveCommit 验证引用可解析为提交，并检查 Git 返回的对象 ID 格式。
func resolveCommit(ctx context.Context, repo, name, ref string) (string, error) { // name 用于区分 base/head 的错误背景。
	output, err := runGit(ctx, repo, "resolve "+name+" commit", // 统一调用 Git，并记录本次操作名称。
		"rev-parse", "--verify", "--end-of-options", ref+"^{commit}") // 验证唯一对象；结束选项解析；要求对象为提交或可剥离为提交的标签。
	if err != nil { // 不存在的引用或无法转为 commit 的对象都会失败。
		return "", err // 失败时不返回 SHA。
	} // Git 执行错误处理结束。
	sha := strings.TrimSpace(string(output)) // 将字节输出转为字符串并去掉外围空白。
	if len(sha) != 40 && len(sha) != 64 {    // 支持 Git SHA-1 的 40 位和 SHA-256 的 64 位完整对象 ID。
		return "", fmt.Errorf("resolve %s commit: Git returned an invalid object ID", name) // 拒绝短 ID 和其他异常输出。
	} // 对象 ID 长度检查结束。
	if _, err := hex.DecodeString(sha); err != nil { // 检查字符是否全部可解析为十六进制，不需要保留解码后的字节。
		return "", fmt.Errorf("resolve %s commit: invalid object ID: %w", name, err) // 长度正确也不能接受非法字符。
	} // 十六进制检查结束。
	return sha, nil // 返回已验证的完整 commit ID。
}

// runGit 用参数数组调用 Git，为每次调用设置超时并限制 stderr 捕获长度。
func runGit(ctx context.Context, repo, operation string, args ...string) ([]byte, error) { // 可变参数 args 表示各个独立命令参数。
	requestCtx, cancel := context.WithTimeout(ctx, gitTimeout) // 创建 10 秒子上下文，同时继承父上下文的更早截止或取消。
	defer cancel()                                             // 无论成功或失败，都释放该上下文关联的定时器等资源。
	if err := requestCtx.Err(); err != nil {                   // 已经取消或超时则不启动子进程。
		return nil, fmt.Errorf("%s: %w", operation, err) // 保留取消或超时错误，便于调用方识别。
	} // 启动前取消检查结束。
	cmd := exec.CommandContext(requestCtx, "git", args...) // 创建一个 *exec.Cmd 对象。直接运行 Git；上下文结束会触发终止进程，参数不经过 shell 展开。
	cmd.Dir = repo                                         // 设置 Git 的工作目录，不修改当前 Go 进程的工作目录。
	cmd.WaitDelay = time.Second                            // 进程退出或上下文结束后，限制等待进程退出及输出管道关闭的额外时间；不是正常运行超时。
	var stdout bytes.Buffer                                // 接收标准输出，成功时作为结果返回；此处没有设置 stdout 大小上限。
	stderr := cappedBuffer{limit: maxStderrBytes}          // 创建有界标准错误缓冲区，超出部分丢弃。
	cmd.Stdout = &stdout                                   // 将子进程标准输出写入内存，而不是当前终端。
	cmd.Stderr = &stderr                                   // 将子进程标准错误写入自定义的截断缓冲区。
	if err := cmd.Run(); err != nil {                      // 启动进程并等待完成；找不到 Git、非零退出等都会返回错误。
		if contextErr := requestCtx.Err(); contextErr != nil { // 若上下文已结束，优先呈现明确的取消或超时原因。
			err = contextErr // 用 context 错误替代进程被终止等较难理解的底层错误。
		} // 上下文错误替换结束。
		detail := strings.TrimSpace(stderr.String()) // 取出有界诊断文本，去掉外围空白，保留可能存在的截断提示。
		if detail != "" {                            // 只有 Git 提供了 stderr 内容时才附加该部分。
			return nil, fmt.Errorf("%s: %w; Git stderr: %s", operation, err, detail) // 同时返回操作、可追踪的错误原因及 Git 诊断。
		} // 有 stderr 的错误处理结束。
		return nil, fmt.Errorf("%s: %w", operation, err) // 没有 stderr 时仍保留操作名称和底层错误。
	} // 进程执行错误处理结束。
	return stdout.Bytes(), nil // 成功时返回标准输出字节，不把 stderr 混入结果。
} // runGit 结束。

// cappedBuffer 接受所有输入，但只保存前 limit 字节，避免 stderr 无限增长。
type cappedBuffer struct { // 使用具名 buffer 字段，不嵌入 bytes.Buffer，避免提升 ReadFrom 等方法绕过 Write 限制。
	buffer    bytes.Buffer // 保存实际留下的字节。
	limit     int          // 可保存的最大字节数。
	truncated bool         // 记录是否曾丢弃超出上限的字节。
} // 有界缓冲区结构定义结束。

// Write 实现 io.Writer；超出的内容会被消费并丢弃，而不是触发写入错误。
func (b *cappedBuffer) Write(p []byte) (int, error) { // 指针接收者让本次写入更新同一个缓冲区。
	remaining := b.limit - b.buffer.Len() // 计算还能保存多少字节。
	if len(p) > remaining {               // 本次输入长度超过剩余空间时会发生截断。
		b.truncated = true // 标记曾有内容被丢弃；后续写入不会清除此标记。
	} // 截断标记检查结束。
	if remaining > 0 { // 缓冲区尚未装满时才保存内容。
		b.buffer.Write(p[:min(len(p), remaining)]) // 截取最多 remaining 字节；内存 bytes.Buffer.Write 不会返回写入错误。
	} // 保存可容纳内容的分支结束。
	return len(p), nil // 声明全部输入已被消费，防止 io.Copy 因返回较小长度而报告短写错误。
} // Write 结束。

// String 返回保留下来的诊断文本，并在发生截断时附加提示。
func (b *cappedBuffer) String() string { // 只读取缓冲区内容，不修改其中的字节。
	if b.truncated { // 判断是否有输入因大小限制被丢弃。
		return b.buffer.String() + " [truncated]" // 提醒调用方诊断不完整；提示文字不占用缓冲区正文额度。
	} // 截断提示分支结束。
	return b.buffer.String() // 未截断时返回完整保存的内容。
} // String 结束。
