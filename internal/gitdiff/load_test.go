// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 测试 Load 函数能正确加载变更集，识别变更类型、处理带空格的路径、统计新增行等。
func TestLoadChangeKindsAndSpacePaths(t *testing.T) {
	repo := newRepo(t)
	for _, name := range []string{"modified.go", "space name.go", "old.go"} {
		writeNamedFile(t, repo, name, "package sample\n// old\n")
	}
	base := commitAll(t, repo)
	updated := "package sample\n// updated\n"
	writeNamedFile(t, repo, "modified.go", updated)
	writeNamedFile(t, repo, "space name.go", updated)
	writeNamedFile(t, repo, "added.go", "package added\n")
	gitTest(t, repo, "mv", "old.go", "renamed.go")
	head := commitAll(t, repo)
	snapshot, err := Resolve(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Load(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path                  string
		kind                  ChangeType
		content               string
		added                 map[int]struct{}
		insertions, deletions int
	}{
		{"added.go", ChangeAdded, "package added\n", map[int]struct{}{1: {}}, 1, 0},
		{"modified.go", ChangeModified, updated, map[int]struct{}{2: {}}, 1, 1},
		{"old.go", ChangeDeleted, "", nil, 0, 0},
		{"renamed.go", ChangeAdded, "package sample\n// old\n", map[int]struct{}{1: {}, 2: {}}, 2, 0},
		{"space name.go", ChangeModified, updated, map[int]struct{}{2: {}}, 1, 1},
	}
	if set.Snapshot != snapshot || len(set.Files) != len(want) || set.HasCoverageGap || len(set.Warnings) != 0 {
		t.Fatalf("unexpected change set: %+v", set)
	}
	for i, expected := range want {
		change := set.Files[i]
		reason := ""
		if expected.kind == ChangeDeleted {
			reason = SkipDeleted
		}
		if change.Path != expected.path || change.Type != expected.kind || change.RawPatch == "" ||
			string(change.HeadContent) != expected.content || !reflect.DeepEqual(change.AddedLines, expected.added) ||
			change.Insertions != expected.insertions || change.Deletions != expected.deletions ||
			change.SkipReason != reason || change.Reviewable != (reason == "") {
			t.Fatalf("change %d = %+v, want %+v, reason %q", i, change, expected, reason)
		}
	}
}

func TestLoadDisplaysNumberedDiff(t *testing.T) {
	repo := newRepo(t)
	baseContent := "package sample\n\nfunc Sum(a, b int) int {\n\treturn a - b\n}\n"
	headContent := "package sample\n\nfunc Sum(a, b int) int {\n\t// Add both inputs.\n\treturn a + b\n}\n"
	base := commitFile(t, repo, baseContent)
	head := commitFile(t, repo, headContent)
	snapshot, err := Resolve(context.Background(), repo, "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != base || snapshot.HeadSHA != head {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	// Leave staged and unstaged replacements; Load must read fixed Git objects.
	writeFile(t, repo, "package staged\n")
	gitTest(t, repo, "add", "sample.go")
	writeFile(t, repo, "package unstaged\n")
	beforeStatus := gitTest(t, repo, "status", "--porcelain")
	beforeIndex := gitTest(t, repo, "diff", "--cached")
	set, err := Load(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if set.Snapshot != snapshot || len(set.Files) != 1 || set.HasCoverageGap || len(set.Warnings) != 0 {
		t.Fatalf("unexpected change set: %+v", set)
	}
	change := set.Files[0]
	if change.Path != "sample.go" || change.Type != ChangeModified || !change.Reviewable || change.SkipReason != "" ||
		string(change.HeadContent) != headContent || change.Insertions != 2 || change.Deletions != 1 ||
		!reflect.DeepEqual(change.AddedLines, map[int]struct{}{4: {}, 5: {}}) {
		t.Fatalf("unexpected loaded file: %+v", change)
	}
	baseLines := strings.Split(strings.TrimSuffix(baseContent, "\n"), "\n")
	headLines := strings.Split(strings.TrimSuffix(string(change.HeadContent), "\n"), "\n")
	for _, line := range change.Lines {
		if line.Kind != LineAdded && (line.OldLine < 1 || line.OldLine > len(baseLines) || baseLines[line.OldLine-1] != line.Content) {
			t.Fatalf("patch line does not match base: %+v", line)
		}
		if line.Kind != LineDeleted && (line.NewLine < 1 || line.NewLine > len(headLines) || headLines[line.NewLine-1] != line.Content) {
			t.Fatalf("patch line does not match head: %+v", line)
		}
	}
	if gitTest(t, repo, "status", "--porcelain") != beforeStatus || gitTest(t, repo, "diff", "--cached") != beforeIndex {
		t.Fatal("Load modified the worktree or index")
	}
	added := make([]int, 0, len(change.AddedLines))
	for number := range change.AddedLines {
		added = append(added, number)
	}
	sort.Ints(added)
	t.Logf("file: %s\nbase SHA: %s\nhead SHA: %s\nnumbered patch:\n%sAddedLines: %v",
		change.Path, snapshot.BaseSHA, snapshot.HeadSHA, displayNumberedPatch(change.ParsedPatch), added)
}

func TestLoadKeepsAllFilesAndReasons(t *testing.T) {
	repo := newRepo(t)
	writeNamedFile(t, repo, "deleted.go", "package sample\n")
	writeNamedFile(t, repo, "trimmed.go", "package sample\n// removed\n")
	base := commitAll(t, repo)
	gitTest(t, repo, "rm", "deleted.go")
	for name, content := range map[string]string{
		"main.go":                 "package sample\n",
		"myvendor/file.go":        "package sample\n",
		"mytestdata/file.go":      "package sample\n",
		"vendor/file.go":          "package excluded\n",
		"nested/testdata/file.go": "package excluded\n",
		"generated.go":            "// Code generated by fixture. DO NOT EDIT.\npackage sample\n",
		"binary.go":               "package sample\x00\n",
		"invalid.go":              "package sample\n// \xff\n",
		"empty.go":                "",
		"trimmed.go":              "package sample\n",
		"notes.md":                "not Go\n",
		"patch-large.go":          "// " + strings.Repeat("x", maxPatchBytes) + "\n",
		"head-large.go":           "// " + strings.Repeat("x", maxHeadFileBytes) + "\n",
	} {
		writeNamedFile(t, repo, name, content)
	}
	gitTest(t, repo, "add", "-A")
	blob := gitTest(t, repo, "rev-parse", base+":deleted.go")
	gitTest(t, repo, "update-index", "--add", "--cacheinfo", "120000,"+blob+",link.go")
	gitTest(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+base+",module.go")
	gitTest(t, repo, "commit", "-m", "Add filtering fixtures")
	snapshot, err := Resolve(context.Background(), repo, base, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// Filtering must inspect head objects rather than this replacement.
	writeNamedFile(t, repo, "generated.go", "package ordinary\n")
	set, err := Load(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"deleted.go": SkipDeleted,
		"main.go":    "", "myvendor/file.go": "", "mytestdata/file.go": "",
		"vendor/file.go": SkipDirectory, "nested/testdata/file.go": SkipDirectory,
		"generated.go": SkipGenerated, "binary.go": SkipBinary, "invalid.go": SkipNonUTF8,
		"empty.go": SkipNoAddedLines, "trimmed.go": SkipNoAddedLines,
		"notes.md": SkipUnsupported, "patch-large.go": SkipPatchTooLarge,
		"head-large.go": SkipHeadTooLarge, "link.go": SkipSpecialFile, "module.go": SkipSpecialFile,
	}
	if set.Snapshot != snapshot || len(set.Files) != len(want) || !set.HasCoverageGap || len(set.Warnings) != 2 {
		t.Fatalf("unexpected change set: %+v", set)
	}
	for _, change := range set.Files {
		reason, exists := want[change.Path]
		if !exists || change.SkipReason != reason || change.Reviewable != (reason == "") {
			t.Fatalf("unexpected selection for %q: reviewable=%v, reason=%q", change.Path, change.Reviewable, change.SkipReason)
		}
		if change.Reviewable && (len(change.AddedLines) == 0 || len(change.HeadContent) == 0) {
			t.Fatalf("reviewable file lacks parsed anchors or head content: %+v", change)
		}
	}
}

func TestStaticPathReason(t *testing.T) {
	for _, tc := range []struct {
		path string
		kind ChangeType
		want string
	}{
		{"deleted.go", ChangeDeleted, SkipDeleted},
		{"README.md", ChangeAdded, SkipUnsupported},
		{"vendor/a.go", ChangeAdded, SkipDirectory},
		{"x/testdata/a.go", ChangeAdded, SkipDirectory},
		{"myvendor/a.go", ChangeAdded, ""},
		{"testdatabase/a.go", ChangeAdded, ""},
		{"space name.go", ChangeAdded, ""},
		{"control\t.go", ChangeAdded, SkipInvalidPath},
		{"../a.go", ChangeAdded, SkipInvalidPath},
	} {
		if got := staticPathReason(FileChange{Path: tc.path, Type: tc.kind}); got != tc.want {
			t.Fatalf("path %q: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestReviewLimitsBoundariesAndOrdering(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		files, lines, patchBytes         int
		headTooLarge, wantError, wantGap bool
		wantReason                       string
	}{
		{name: "eight files", files: 8, lines: 8},
		{name: "nine files", files: 9, lines: 9, wantError: true, wantGap: true, wantReason: SkipRangeTooLarge},
		{name: "four hundred lines", files: 1, lines: 400},
		{name: "four hundred one lines", files: 1, lines: 401, wantError: true, wantGap: true, wantReason: SkipRangeTooLarge},
		{name: "exact patch limit", files: 1, lines: 1, patchBytes: maxPatchBytes},
		{name: "oversized patch", files: 1, lines: 1, patchBytes: maxPatchBytes + 1, wantGap: true, wantReason: SkipPatchTooLarge},
		{name: "global files before patch skip", files: 9, lines: 9, patchBytes: maxPatchBytes + 1, wantError: true, wantGap: true, wantReason: SkipRangeTooLarge},
		{name: "global lines before patch skip", files: 1, lines: 401, patchBytes: maxPatchBytes + 1, wantError: true, wantGap: true, wantReason: SkipRangeTooLarge},
		{name: "head size gap", files: 1, lines: 1, headTooLarge: true, wantGap: true, wantReason: SkipHeadTooLarge},
		{name: "head size still counts globally", files: 9, lines: 9, headTooLarge: true, wantError: true, wantGap: true, wantReason: SkipRangeTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes := make([]FileChange, tc.files)
			for i := range changes {
				changes[i] = FileChange{Path: fmt.Sprintf("f%d.go", i), RawPatch: strings.Repeat("x", tc.patchBytes), ParsedPatch: ParsedPatch{Insertions: 1}}
				if tc.headTooLarge {
					changes[i].SkipReason = SkipHeadTooLarge
				}
			}
			changes[0].Insertions += tc.lines - tc.files
			warnings, gap, err := applyReviewLimits(changes)
			if errors.Is(err, ErrRangeTooLarge) != tc.wantError || gap != tc.wantGap || changes[0].SkipReason != tc.wantReason || changes[0].Reviewable != (tc.wantReason == "") {
				t.Fatalf("reason=%q, reviewable=%v, gap=%v, error=%v", changes[0].SkipReason, changes[0].Reviewable, gap, err)
			}
			if tc.wantError && (len(warnings) != 0 || !strings.Contains(err.Error(), "narrow the comparison range")) {
				t.Fatalf("global limit was applied after per-file skips: warnings %v, error %v", warnings, err)
			}
			if !tc.wantError && tc.wantGap && len(warnings) == 0 {
				t.Fatal("size gap did not produce a warning")
			}
		})
	}
}

func TestLoadEmptyStaticSkipsAndErrors(t *testing.T) {
	repo := newRepo(t)
	base := commitFile(t, repo, "package sample\n")
	writeNamedFile(t, repo, "notes.md", "only documentation\n")
	head := commitAll(t, repo)
	snapshot, err := Resolve(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Load(context.Background(), snapshot)
	if err != nil || len(set.Files) != 1 || set.Files[0].SkipReason != SkipUnsupported || set.HasCoverageGap || len(set.Warnings) != 0 {
		t.Fatalf("static skips should not create a gap: %+v, error %v", set, err)
	}
	snapshot.BaseSHA = head
	set, err = Load(context.Background(), snapshot)
	if err != nil || set.Files == nil || len(set.Files) != 0 || set.Warnings == nil || set.HasCoverageGap {
		t.Fatalf("empty input: %+v, error %v", set, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("load cancellation: %v", err)
	}
	if _, err := Load(context.Background(), Snapshot{}); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
}

func TestLoadRejectsWholeOversizedRange(t *testing.T) {
	repo := newRepo(t)
	base := commitFile(t, repo, "package sample\n")
	for i := 0; i < 9; i++ {
		writeNamedFile(t, repo, fmt.Sprintf("added%d.go", i), "package sample\n")
	}
	head := commitAll(t, repo)
	snapshot, err := Resolve(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Load(context.Background(), snapshot)
	if !errors.Is(err, ErrRangeTooLarge) || len(set.Files) != 9 || !set.HasCoverageGap {
		t.Fatalf("oversized range: files=%d, gap=%v, error=%v", len(set.Files), set.HasCoverageGap, err)
	}
	for _, change := range set.Files {
		if change.Reviewable || change.SkipReason != SkipRangeTooLarge {
			t.Fatalf("range was silently reduced: %+v", change)
		}
	}
}

func TestReviewLimitsCountDeletionsAndExcludeStaticSkips(t *testing.T) {
	changes := []FileChange{{Path: "f.go", ParsedPatch: ParsedPatch{Insertions: 1, Deletions: 400}}}
	if _, _, err := applyReviewLimits(changes); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("deletions were not counted: %v", err)
	}
	changes = []FileChange{{Path: "generated.go", SkipReason: SkipGenerated, ParsedPatch: ParsedPatch{Insertions: 1000}}}
	warnings, gap, err := applyReviewLimits(changes)
	if err != nil || gap || len(warnings) != 0 || changes[0].Reviewable {
		t.Fatalf("static skip consumed budget: warnings=%v, gap=%v, error=%v", warnings, gap, err)
	}
}
