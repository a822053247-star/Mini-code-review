// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: .*)?$`)

// ParsePatch maps one file's unified patch to old and new line numbers.
// 函数把 Git patch 文本转换成带行号的结构化数据。
// 函数作用：解析 Git 补丁文本，返回每行的变更类型、旧行号、新行号和内容，以及插入/删除统计信息。
func ParsePatch(raw string) (ParsedPatch, error) {
	result := ParsedPatch{Lines: []PatchLine{}, AddedLines: map[int]struct{}{}}
	lines := strings.Split(raw, "\n")
	if strings.HasSuffix(raw, "\n") {
		lines = lines[:len(lines)-1]
	}
	var oldLine, newLine, oldRemaining, newRemaining int
	inHunk, canMarkNoNewline, binarySection := false, false, false
	checkCounts := func() error {
		if inHunk && (oldRemaining != 0 || newRemaining != 0) {
			return fmt.Errorf("hunk line counts do not match: %d old and %d new lines missing", oldRemaining, newRemaining)
		}
		return nil
	}
	for index, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\r")
		fail := func(reason string) (ParsedPatch, error) {
			return ParsedPatch{}, fmt.Errorf("patch line %d: %s", index+1, reason)
		}
		if strings.HasPrefix(line, "diff --git ") {
			if err := checkCounts(); err != nil {
				return fail(err.Error())
			}
			inHunk, canMarkNoNewline, binarySection = false, false, false
			continue
		}
		if binarySection {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			if err := checkCounts(); err != nil {
				return fail(err.Error())
			}
			matches := hunkHeader.FindStringSubmatch(line)
			if matches == nil {
				return fail("invalid or unsupported hunk header")
			}
			var err error
			oldLine, oldRemaining, err = parseHunkRange(matches[1], matches[2])
			if err != nil {
				return fail(err.Error())
			}
			newLine, newRemaining, err = parseHunkRange(matches[3], matches[4])
			if err != nil {
				return fail(err.Error())
			}
			inHunk, canMarkNoNewline = true, false
			continue
		}
		if line == `\ No newline at end of file` {
			if !inHunk || !canMarkNoNewline {
				return fail("no-newline marker must follow a hunk content line")
			}
			canMarkNoNewline = false
			continue
		}
		if !inHunk {
			if (strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ")) || line == "GIT binary patch" {
				result.IsBinary, binarySection = true, true
			}
			continue
		}
		if line == "" {
			return fail("hunk content is missing a line prefix")
		}
		entry := PatchLine{Kind: LineKind(line[:1]), Content: line[1:]}
		switch entry.Kind {
		case LineContext:
			if oldRemaining <= 0 || newRemaining <= 0 {
				return fail("context line exceeds hunk counts")
			}
			entry.OldLine, entry.NewLine = oldLine, newLine
			oldLine, newLine = oldLine+1, newLine+1
			oldRemaining, newRemaining = oldRemaining-1, newRemaining-1
		case LineAdded:
			if newRemaining <= 0 {
				return fail("added line exceeds hunk count")
			}
			entry.NewLine = newLine
			result.AddedLines[newLine] = struct{}{}
			newLine, newRemaining = newLine+1, newRemaining-1
			result.Insertions++
		case LineDeleted:
			if oldRemaining <= 0 {
				return fail("deleted line exceeds hunk count")
			}
			entry.OldLine = oldLine
			oldLine, oldRemaining = oldLine+1, oldRemaining-1
			result.Deletions++
		default:
			return fail("invalid hunk content prefix")
		}
		result.Lines = append(result.Lines, entry)
		canMarkNoNewline = true
	}
	if err := checkCounts(); err != nil {
		return ParsedPatch{}, err
	}
	return result, nil
}

func parseHunkRange(startText, countText string) (int, int, error) {
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid hunk start: %w", err)
	}
	count := 1
	if countText != "" {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid hunk count: %w", err)
		}
	}
	maxInt := int(^uint(0) >> 1)
	if (start == 0 && count != 0) || count > maxInt-start {
		return 0, 0, fmt.Errorf("invalid or overflowing hunk range")
	}
	return start, count, nil
}
