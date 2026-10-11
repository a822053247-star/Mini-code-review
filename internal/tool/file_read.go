// SPDX-License-Identifier: Apache-2.0

package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"example.com/mini-code-review/internal/gitdiff"
)

const (
	defaultReadLines = 120
	maxReadLines     = 200
	maxReadBytes     = 16 * 1024
)

// HeadFileReader 是唯一需要替换的读取依赖，生产默认读取固定 head 的 Git 对象。
type HeadFileReader func(context.Context, gitdiff.Snapshot, string) ([]byte, error)

// FileReader 保存 Snapshot 的值副本，不读取工作区或重新解析分支。
type FileReader struct {
	snapshot gitdiff.Snapshot
	read     HeadFileReader
}

// NewFileReader 接收已解析的 Snapshot；read 为 nil 时使用 ReadHeadFile。
func NewFileReader(snapshot gitdiff.Snapshot, read HeadFileReader) (*FileReader, error) {
	if err := gitdiff.ValidateSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("configure file_read: %w", err)
	}
	if read == nil {
		read = gitdiff.ReadHeadFile
	}
	return &FileReader{snapshot: snapshot, read: read}, nil
}

// FileReadData 的行号表示实际返回范围，空文件两端为 null。
// 字节预算只统计带行号的 Content，不包含元数据和 JSON 转义。
type FileReadData struct {
	Path              string `json:"path"`
	HeadSHA           string `json:"head_sha"`
	Content           string `json:"content"`
	TotalLines        int    `json:"total_lines"`
	StartLine         *int   `json:"start_line"`
	EndLine           *int   `json:"end_line"`
	Truncated         bool   `json:"truncated"`
	LastLineTruncated bool   `json:"last_line_truncated"`
}

// SplitSourceLines 供读取和后续评论计数共用：末尾换行不额外产生一行。
func SplitSourceLines(content []byte) []string {
	if len(content) == 0 {
		return []string{}
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// Execute 可直接绑定到 Handlers.FileRead；它不需要 patch 或 AddedLines。
func (reader *FileReader) Execute(ctx context.Context, args FileReadArgs) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("file_read: %w", err)
	}
	if reason := gitdiff.SourcePathReason(args.Path); reason != "" {
		code := "file_excluded"
		if reason == gitdiff.SkipInvalidPath {
			code = "invalid_path"
		}
		return Failure(code, "source path is not allowed: "+reason), nil
	}
	content, err := reader.read(ctx, reader.snapshot, args.Path)
	if contextErr := ctx.Err(); contextErr != nil {
		return Execution{}, fmt.Errorf("file_read: %w", contextErr)
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Execution{}, fmt.Errorf("file_read: %w", err)
	case errors.Is(err, gitdiff.ErrNotRegularFile):
		return Failure("special_file", "head path is not a regular source file"), nil
	case errors.Is(err, gitdiff.ErrFileTooLarge):
		return Failure("file_too_large", "head file exceeds 256 KiB"), nil
	case err != nil:
		return Failure("read_failed", "head content could not be read"), nil
	}
	// 替代读取函数也必须遵守相同的整体大小限制。
	if len(content) > gitdiff.MaxHeadFileBytes {
		return Failure("file_too_large", "head file exceeds 256 KiB"), nil
	}
	if reason := gitdiff.SourceContentReason(content); reason != "" {
		return Failure("file_excluded", "source content is not allowed: "+reason), nil
	}
	lines := SplitSourceLines(content)
	data := FileReadData{Path: args.Path, HeadSHA: reader.snapshot.HeadSHA, TotalLines: len(lines)}
	if len(lines) == 0 {
		if args.StartLine != nil || args.EndLine != nil {
			return Failure("invalid_range", "empty files have no readable line range"), nil
		}
		return Success(data), nil
	}
	start := 1
	if args.StartLine != nil {
		start = *args.StartLine
	}
	if start < 1 || start > len(lines) {
		return Failure("invalid_range", "start_line is outside the head file"), nil
	}
	end := len(lines)
	if args.EndLine != nil {
		end = *args.EndLine
		if end < start || end > len(lines) {
			return Failure("invalid_range", "end_line must be within the file and not precede start_line"), nil
		}
	} else if len(lines)-start+1 > defaultReadLines {
		// 先校验 start 并比较剩余行数，避免默认窗口计算发生整数溢出。
		end = start + defaultReadLines - 1
	}
	count := min(end-start+1, maxReadLines)
	var numbered strings.Builder
	for offset := 0; offset < count; offset++ {
		if err := ctx.Err(); err != nil {
			return Execution{}, fmt.Errorf("file_read: %w", err)
		}
		number := start + offset
		prefix := fmt.Sprintf("%d|", number)
		line := lines[number-1]
		remaining := maxReadBytes - numbered.Len()
		if len(prefix)+len(line)+1 > remaining {
			if offset > 0 {
				break
			}
			// 第一行过长时只返回完整 UTF-8 字符前缀，预留展示换行。
			cut := remaining - len(prefix) - 1
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
			data.LastLineTruncated = true
		}
		numbered.WriteString(prefix)
		numbered.WriteString(line)
		numbered.WriteByte('\n')
		if data.StartLine == nil {
			first := number
			data.StartLine = &first
		}
		last := number
		data.EndLine = &last
		if data.LastLineTruncated {
			break
		}
	}
	data.Content = numbered.String()
	data.Truncated = *data.EndLine < end || data.LastLineTruncated
	if err := ctx.Err(); err != nil {
		return Execution{}, fmt.Errorf("file_read: %w", err)
	}
	return Success(data), nil
}
