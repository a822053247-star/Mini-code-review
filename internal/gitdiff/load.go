// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxReviewFiles    = 8
	maxChangedLines   = 400
	maxPatchBytes     = 32 * 1024
	SkipDeleted       = "deleted"
	SkipUnsupported   = "unsupported_extension"
	SkipDirectory     = "excluded_directory"
	SkipInvalidPath   = "invalid_path"
	SkipGenerated     = "generated"
	SkipBinary        = "binary"
	SkipNonUTF8       = "non_utf8"
	SkipSpecialFile   = "special_file"
	SkipNoAddedLines  = "no_added_lines"
	SkipHeadTooLarge  = "head_too_large"
	SkipPatchTooLarge = "patch_too_large"
	SkipRangeTooLarge = "range_too_large"
)

var (
	ErrRangeTooLarge = errors.New("review range exceeds limits; narrow the comparison range")
	generatedMarker  = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.\r?$`)
)

// 统一gitdiff的入口，负责：获取变更 → 解析 patch → 读取内容 → 筛选文件 → 检查范围限制
// 最后返回 ChangeSet，里面既有可审查文件，也有被跳过的文件及原因。
// 返回结果是ChangeSet，包含：
/*
ChangeSet{
	Snapshot: snapshot,

	Files: []FileChange{
		{
			Path:       "README.md",
			Type:       ChangeModified, // "M"
			RawPatch:   "...README 的 patch...",
			HeadContent: nil,
			Reviewable: false,
			SkipReason: "unsupported_extension",
			// ParsedPatch 未解析，保持零值
		},
		{
			Path:     "sum.go",
			Type:     ChangeModified, // "M"
			RawPatch: "...sum.go 的 patch...",

			ParsedPatch: ParsedPatch{
				Lines:      行号信息,
				AddedLines: map[int]struct{}{4: struct{}{}},
				Insertions: 1,
				Deletions:  1,
				IsBinary:   false,
			},

			HeadContent: []byte(
				"package demo\n\n" +
				"func Sum(a, b int) int {\n" +
				"\treturn a + b\n" +
				"}\n",
			),

			Reviewable: true,
			SkipReason: "",
		},
	},

	Warnings:       []string{},
	HasCoverageGap: false,
}
*/
func Load(ctx context.Context, snapshot Snapshot) (ChangeSet, error) {
	changes, err := GetChanges(ctx, snapshot)
	if err != nil {
		return ChangeSet{}, err
	}
	set := ChangeSet{Snapshot: snapshot, Files: changes, Warnings: []string{}}
	for i := range set.Files {
		if err := ctx.Err(); err != nil {
			return ChangeSet{}, err
		}
		change := &set.Files[i]
		if change.SkipReason = staticPathReason(*change); change.SkipReason != "" {
			continue
		}
		parsed, err := ParsePatch(change.RawPatch)
		if err != nil {
			return ChangeSet{}, fmt.Errorf("parse patch %q: %w", change.Path, err)
		}
		change.ParsedPatch = parsed
		if parsed.IsBinary {
			change.SkipReason = SkipBinary
			continue
		}
		content, err := ReadHeadFile(ctx, snapshot, change.Path)
		switch {
		case errors.Is(err, ErrNotRegularFile):
			change.SkipReason = SkipSpecialFile
			continue
		case errors.Is(err, ErrFileTooLarge):
			// Keep this candidate in the global budget despite the read limit.
			change.SkipReason = SkipHeadTooLarge
		case err != nil:
			return ChangeSet{}, fmt.Errorf("load head content %q: %w", change.Path, err)
		default:
			change.HeadContent = content
			if bytes.IndexByte(content, 0) >= 0 {
				change.SkipReason = SkipBinary
			} else if !utf8.Valid(content) {
				change.SkipReason = SkipNonUTF8
			} else if generatedMarker.Match(content) {
				change.SkipReason = SkipGenerated
			}
		}
		if change.SkipReason == "" && len(parsed.AddedLines) == 0 {
			change.SkipReason = SkipNoAddedLines
		}
	}
	set.Warnings, set.HasCoverageGap, err = applyReviewLimits(set.Files)
	return set, err
}

func staticPathReason(change FileChange) string {
	if change.Type == ChangeDeleted {
		return SkipDeleted
	}
	if validateFilePath(change.Path) != nil {
		return SkipInvalidPath
	}
	if !strings.HasSuffix(change.Path, ".go") {
		return SkipUnsupported
	}
	parts := strings.Split(change.Path, "/")
	for _, directory := range parts[:len(parts)-1] {
		if directory == "vendor" || directory == "testdata" {
			return SkipDirectory
		}
	}
	return ""
}

func applyReviewLimits(changes []FileChange) ([]string, bool, error) {
	warnings := []string{}
	candidates, changedLines := 0, 0
	for _, change := range changes {
		if change.SkipReason == "" || change.SkipReason == SkipHeadTooLarge {
			candidates++
			changedLines += change.Insertions + change.Deletions
		}
	}
	if candidates > maxReviewFiles || changedLines > maxChangedLines {
		for i := range changes {
			if changes[i].SkipReason == "" || changes[i].SkipReason == SkipHeadTooLarge {
				changes[i].Reviewable = false
				changes[i].SkipReason = SkipRangeTooLarge
			}
		}
		return warnings, true, fmt.Errorf("%w: %d candidate files and %d changed lines (maximum %d files and %d lines)", ErrRangeTooLarge, candidates, changedLines, maxReviewFiles, maxChangedLines)
	}
	gap := false
	for i := range changes {
		change := &changes[i]
		if change.SkipReason == "" && len(change.RawPatch) > maxPatchBytes {
			change.SkipReason = SkipPatchTooLarge
		}
		change.Reviewable = change.SkipReason == ""
		if change.SkipReason == SkipPatchTooLarge || change.SkipReason == SkipHeadTooLarge {
			gap = true
			warnings = append(warnings, fmt.Sprintf("%s: %s; review coverage is incomplete", change.Path, change.SkipReason))
		}
	}
	return warnings, gap, nil
}
