// SPDX-License-Identifier: Apache-2.0

package gitdiff

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParsePatchLineMapping(t *testing.T) {
	for _, tc := range []struct {
		name, patch           string
		lines                 []PatchLine
		added                 []int
		insertions, deletions int
	}{
		{
			name:  "multiple hunks reset both sides",
			patch: "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -2,3 +2,4 @@ section\n keep\n-old\n+new\n+extra\n tail\n@@ -20 +21 @@\n-last\n+replacement\n",
			lines: []PatchLine{
				{LineContext, 2, 2, "keep"}, {LineDeleted, 3, 0, "old"},
				{LineAdded, 0, 3, "new"}, {LineAdded, 0, 4, "extra"},
				{LineContext, 4, 5, "tail"}, {LineDeleted, 20, 0, "last"},
				{LineAdded, 0, 21, "replacement"},
			},
			added: []int{3, 4, 21}, insertions: 3, deletions: 2,
		},
		{
			name:  "new file with zero old count",
			patch: "--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,2 @@\n+package sample\n+\n",
			lines: []PatchLine{{LineAdded, 0, 1, "package sample"}, {LineAdded, 0, 2, ""}},
			added: []int{1, 2}, insertions: 2,
		},
		{
			name:      "deleted file with zero new count",
			patch:     "--- a/old.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package sample\n-\n",
			lines:     []PatchLine{{LineDeleted, 1, 0, "package sample"}, {LineDeleted, 2, 0, ""}},
			deletions: 2,
		},
		{
			name:  "omitted counts and no final newline",
			patch: "@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file",
			lines: []PatchLine{{LineDeleted, 1, 0, "old"}, {LineAdded, 0, 1, "new"}},
			added: []int{1}, insertions: 1, deletions: 1,
		},
		{
			name:  "prefixes in content are not file headers",
			patch: "--- a/f.go\n+++ b/f.go\n@@ -1,2 +1,3 @@\n---old\n+++new\n+-minus\n Binary files text in source\n",
			lines: []PatchLine{{LineDeleted, 1, 0, "--old"}, {LineAdded, 0, 1, "++new"}, {LineAdded, 0, 2, "-minus"}, {LineContext, 2, 3, "Binary files text in source"}},
			added: []int{1, 2}, insertions: 2, deletions: 1,
		},
		{
			name:  "CRLF preserves interior carriage return and spaces",
			patch: "@@ -1 +1 @@\r\n-old\r\n+  new\rtext  \r\n",
			lines: []PatchLine{{LineDeleted, 1, 0, "old"}, {LineAdded, 0, 1, "  new\rtext  "}},
			added: []int{1}, insertions: 1, deletions: 1,
		},
		{
			name:  "empty file has metadata only",
			patch: "diff --git a/empty.go b/empty.go\nnew file mode 100644\nindex 0000000..e69de29\n",
		},
		{
			name: "empty patch", patch: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParsePatch(tc.patch)
			if err != nil {
				t.Fatal(err)
			}
			wantLines := tc.lines
			if wantLines == nil {
				wantLines = []PatchLine{}
			}
			wantAdded := map[int]struct{}{}
			for _, number := range tc.added {
				wantAdded[number] = struct{}{}
			}
			if !reflect.DeepEqual(parsed.Lines, wantLines) || !reflect.DeepEqual(parsed.AddedLines, wantAdded) || parsed.Insertions != tc.insertions || parsed.Deletions != tc.deletions || parsed.IsBinary {
				t.Fatalf("parsed = %+v; want lines %+v, added %v, insertions %d, deletions %d", parsed, wantLines, wantAdded, tc.insertions, tc.deletions)
			}
		})
	}
}

func TestParseBinaryPatch(t *testing.T) {
	for _, patch := range []string{
		"diff --git a/f b/f\nBinary files a/f and b/f differ\n",
		"diff --git a/f b/f\nGIT binary patch\nliteral 10\nencoded-payload\n",
	} {
		parsed, err := ParsePatch(patch)
		if err != nil || !parsed.IsBinary || len(parsed.Lines) != 0 || len(parsed.AddedLines) != 0 || parsed.Insertions != 0 || parsed.Deletions != 0 {
			t.Fatalf("binary patch = %+v, error %v", parsed, err)
		}
	}
}

func TestParsePatchRejectsMalformedHunks(t *testing.T) {
	for _, patch := range []string{
		"@@ not a hunk @@\n",
		"@@@ -1 +1 @@@\n",
		"@@ -0 +1 @@\n-x\n+y\n",
		"@@ -1 +0 @@\n-x\n+y\n",
		"@@ -999999999999999999999999999999 +1 @@\n",
		"@@ -1,999999999999999999999999999999 +1 @@\n",
		"@@ -1,2 +1,2 @@\n a\n",
		"@@ -1 +1 @@\n a\n+extra\n",
		"@@ -1 +1 @@\n a\n-extra\n",
		"@@ -1 +1 @@\n a\n extra\n",
		"@@ -1 +1 @@\n?bad prefix\n",
		"@@ -1 +1 @@\n\n",
		"@@ -1,2 +1,2 @@\n a\n@@ -10 +10 @@\n b\n",
		"@@ -1,2 +1,2 @@\n a\ndiff --git a/next b/next\n",
		"\\ No newline at end of file\n",
		"@@ -1 +1 @@\n\\ No newline at end of file\n",
	} {
		parsed, err := ParsePatch(patch)
		if err == nil || !reflect.DeepEqual(parsed, ParsedPatch{}) {
			t.Fatalf("malformed patch returned mappings: %q, %+v, error %v", patch, parsed, err)
		}
	}
}

func TestParseRealPatchMatchesHeadLines(t *testing.T) {
	repo := newRepo(t)
	var old strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&old, "// line %d\n", i)
	}
	base := commitFile(t, repo, old.String())
	newContent := strings.Replace(old.String(), "// line 2\n", "// replacement 2\n// inserted 3\n", 1)
	newContent = strings.Replace(newContent, "// line 25\n", "// replacement 25\n", 1)
	head := commitFile(t, repo, newContent)
	snapshot, err := Resolve(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := GetChanges(context.Background(), snapshot)
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes = %+v, error %v", changes, err)
	}
	if strings.Count(changes[0].RawPatch, "@@ -") != 2 {
		t.Fatalf("expected two real hunks: %s", changes[0].RawPatch)
	}
	parsed, err := ParsePatch(changes[0].RawPatch)
	if err != nil {
		t.Fatal(err)
	}
	content, err := ReadHeadFile(context.Background(), snapshot, "sample.go")
	if err != nil {
		t.Fatal(err)
	}
	headLines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	wantAdded := map[int]struct{}{2: {}, 3: {}, 26: {}}
	if !reflect.DeepEqual(parsed.AddedLines, wantAdded) || parsed.Insertions != 3 || parsed.Deletions != 2 {
		t.Fatalf("unexpected real line mapping: %+v", parsed)
	}
	for _, line := range parsed.Lines {
		if line.Kind != LineDeleted && (line.NewLine < 1 || line.NewLine > len(headLines) || headLines[line.NewLine-1] != line.Content) {
			t.Fatalf("patch line does not match head: %+v", line)
		}
	}
	t.Logf("numbered patch:\n%s", displayNumberedPatch(parsed))
}

// displayNumberedPatch is a development aid, not the final report renderer.
func displayNumberedPatch(parsed ParsedPatch) string {
	var output strings.Builder
	for _, line := range parsed.Lines {
		switch line.Kind {
		case LineAdded:
			fmt.Fprintf(&output, "+ head:%d %s\n", line.NewLine, line.Content)
		case LineDeleted:
			fmt.Fprintf(&output, "- base:%d %s\n", line.OldLine, line.Content)
		case LineContext:
			fmt.Fprintf(&output, "  base:%d head:%d %s\n", line.OldLine, line.NewLine, line.Content)
		}
	}
	return output.String()
}
