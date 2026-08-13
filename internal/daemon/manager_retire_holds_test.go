package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestHandlePushReceived_RetiresMergedCustodyHolds is the release-owner
// regression: a push notification whose new tip contains a prior run's hold
// commit as an ancestor must retire that hold, so custody debt does not
// accumulate forever. The hold is created directly via custody.AnchorCurrent
// to simulate a completed run whose worktree was removed and anchored,
// independent of exercising the removal path itself (covered by
// custody_guard_static_test.go and the Commit 2 worktree tests).
func TestHandlePushReceived_RetiresMergedCustodyHolds(t *testing.T) {
	ctx := context.Background()
	p, database := newRefreshRunFixture(t)
	repo, headSHA := setupTestGitRepo(t, p, database, "retire-holds-repo")
	gateDir := p.RepoDir(repo.ID)

	mergedRunID := "01MERGEDTESTHDPADXXXXXXXXX"
	if err := custody.AnchorCurrent(ctx, gateDir, mergedRunID, headSHA); err != nil {
		t.Fatalf("anchor hold: %v", err)
	}

	// Advance main past the held commit and push it to the gate, as a real
	// merge would.
	if err := os.WriteFile(repo.WorkingPath+"/advance.txt", []byte("advance"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "advance past held commit")
	newTip := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")

	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	t.Cleanup(manager.Shutdown)

	runID, err := manager.HandlePushReceived(ctx, &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/main",
		Old:  headSHA,
		New:  newTip,
	})
	if err != nil {
		t.Fatalf("HandlePushReceived: %v", err)
	}

	if _, err := git.ResolveRef(ctx, gateDir, custody.HoldRef(mergedRunID)); err == nil {
		t.Fatalf("merged hold %s should have been retired by the push notification", custody.HoldRef(mergedRunID))
	}

	waitForRunTerminalState(t, database, runID)
}
