package git

import (
	"context"
	"path/filepath"
	"strings"
)

// WorktreeEntry is one entry from `git worktree list --porcelain`. Git keeps
// the administrative entry for a worktree even after its directory has been
// deleted, until an explicit `git worktree prune` removes it - so this is the
// registration-authoritative view of what worktrees exist, independent of
// what is actually present on disk right now.
type WorktreeEntry struct {
	Path     string
	HEAD     string
	Branch   string // refs/heads/<name>, empty when Detached
	Detached bool
	Bare     bool
}

// WorktreeList returns every worktree registered against repoDir (a gate
// bare repo), via `git worktree list --porcelain`.
func WorktreeList(ctx context.Context, repoDir string) ([]WorktreeEntry, error) {
	out, err := Run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

func parseWorktreeList(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var cur *WorktreeEntry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &WorktreeEntry{Path: filepath.Clean(strings.TrimPrefix(line, "worktree "))}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			cur.HEAD = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(line, "branch ")
		case line == "detached":
			cur.Detached = true
		case line == "bare":
			cur.Bare = true
		}
	}
	flush()
	return entries
}
