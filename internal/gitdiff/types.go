// SPDX-License-Identifier: Apache-2.0

// Package gitdiff reads changes from fixed Git commit versions.
package gitdiff

// Snapshot pins a comparison to resolved commits rather than moving refs.
type Snapshot struct {
	RepoRoot string
	BaseSHA  string
	HeadSHA  string
}
