// SPDX-License-Identifier: Apache-2.0

// Package gitdiff reads changes from fixed Git commit versions.
package gitdiff

// Snapshot pins a comparison to resolved commits rather than moving refs.
type Snapshot struct {
	RepoRoot string
	BaseSHA  string
	HeadSHA  string
}

// ChangeType identifies a change between two commit trees.
type ChangeType string

const (
	ChangeAdded       ChangeType = "A"
	ChangeModified    ChangeType = "M"
	ChangeDeleted     ChangeType = "D"
	ChangeTypeChanged ChangeType = "T"
)

// FileChange retains the raw patch; ParsedPatch is populated by the caller.
type FileChange struct {
	Path     string
	Type     ChangeType
	RawPatch string
	ParsedPatch
}

// LineKind is the prefix identifying a unified patch content line.
type LineKind string

const (
	LineContext LineKind = " "
	LineAdded   LineKind = "+"
	LineDeleted LineKind = "-"
)

// PatchLine stores content without its prefix; an absent side has line zero.
type PatchLine struct {
	Kind    LineKind
	OldLine int
	NewLine int
	Content string
}

// ParsedPatch contains validated line mappings and insertion/deletion totals.
type ParsedPatch struct {
	Lines      []PatchLine
	AddedLines map[int]struct{}
	Insertions int
	Deletions  int
	IsBinary   bool
}
