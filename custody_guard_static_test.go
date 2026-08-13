package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This pins the single-worktree-handoff invariant: every production removal
// of a run worktree must go through custody.AnchorAndRemoveRunWorktree, the
// one place that anchors a hold ref before deleting, and no production code
// may fall back to a raw directory delete of a worktree path. See the
// "Guarded Local Branch Synchronization" custody model in AGENTS.md.
func TestCustodyGuard_WorktreeRemovalIsSingleChokePoint(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	internalDir := filepath.Join(root, "internal")

	var removeCallers []string
	var daemonPipelineRemoveAll []string

	walkErr := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		rel := strings.TrimPrefix(path, root+string(filepath.Separator))
		if strings.Contains(content, "git.WorktreeRemove(") {
			removeCallers = append(removeCallers, rel)
		}
		if (strings.HasPrefix(rel, filepath.Join("internal", "daemon")) || strings.HasPrefix(rel, filepath.Join("internal", "pipeline"))) &&
			strings.Contains(content, "os.RemoveAll(") {
			daemonPipelineRemoveAll = append(daemonPipelineRemoveAll, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	wantCaller := filepath.Join("internal", "custody", "custody.go")
	if len(removeCallers) != 1 || removeCallers[0] != wantCaller {
		t.Fatalf("git.WorktreeRemove production callers = %v, want exactly [%s]", removeCallers, wantCaller)
	}
	if len(daemonPipelineRemoveAll) != 0 {
		t.Fatalf("internal/daemon and internal/pipeline must never raw-delete a worktree path, found os.RemoveAll in %v", daemonPipelineRemoveAll)
	}
}
