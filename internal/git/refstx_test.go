package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func newBareRepoWithCommit(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	if _, err := runInDir(context.Background(), work, "-c", "init.defaultBranch=main", "init"); err != nil {
		t.Fatalf("init work: %v", err)
	}
	if _, err := runInDir(context.Background(), work, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "seed"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sha, err := runInDir(context.Background(), work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if _, err := runInDir(context.Background(), work, "push", dir, "HEAD:refs/heads/main"); err != nil {
		t.Fatalf("push seed: %v", err)
	}
	return dir, sha
}

func commitOn(t *testing.T, dir, parentSHA string) string {
	t.Helper()
	// A distinct message is required: commit-tree on the same parent/tree
	// with an identical message and author timestamp produces an identical
	// commit SHA, which would silently collapse "two different commits" into
	// one in a fast-running test.
	msg := "next-" + strings.TrimSpace(fmt.Sprintf("%d", commitOnCounter.Add(1)))
	sha, err := Run(context.Background(), dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit-tree", "-p", parentSHA, "-m", msg, parentSHA+"^{tree}")
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	return strings.TrimSpace(sha)
}

var commitOnCounter atomic.Int64

func TestUpdateRefsTx_CreatesRefAtomically(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	ref := "refs/no-mistakes/hold/01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := UpdateRefsTx(context.Background(), dir, []RefTxOp{{Verb: "update", Ref: ref, NewOID: sha, OldOID: ZeroOID}}); err != nil {
		t.Fatalf("UpdateRefsTx: %v", err)
	}
	got, err := ResolveRef(context.Background(), dir, ref)
	if err != nil || got != sha {
		t.Fatalf("hold ref = %q, %v; want %s", got, err, sha)
	}
}

func TestUpdateRefsTx_CASRejectsStaleOldOID(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	ref := "refs/no-mistakes/hold/01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := UpdateRefsTx(context.Background(), dir, []RefTxOp{{Verb: "update", Ref: ref, NewOID: sha, OldOID: ZeroOID}}); err != nil {
		t.Fatalf("seed update: %v", err)
	}
	// A stale writer expects the ref to still be absent; it must not be able
	// to clobber the value another writer already committed.
	if err := UpdateRefsTx(context.Background(), dir, []RefTxOp{{Verb: "update", Ref: ref, NewOID: sha, OldOID: ZeroOID}}); err == nil {
		t.Fatalf("expected stale-old-OID update to be rejected")
	}
	got, err := ResolveRef(context.Background(), dir, ref)
	if err != nil || got != sha {
		t.Fatalf("ref must remain at %s after rejected stale write, got %q, %v", sha, got, err)
	}
}

func TestUpdateRefsTx_AtomicAllOrNothing(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	refA := "refs/no-mistakes/hold/01ARZ3NDEKTSV4RRFFQ69G5FAA"
	refB := "refs/heads/main"
	ops := []RefTxOp{
		{Verb: "update", Ref: refA, NewOID: sha, OldOID: ZeroOID},
		// Wrong expected-old for refB deliberately, so the whole transaction
		// must abort - including refA, which would otherwise be created.
		{Verb: "verify", Ref: refB, OldOID: ZeroOID},
	}
	if err := UpdateRefsTx(context.Background(), dir, ops); err == nil {
		t.Fatalf("expected transaction to fail")
	}
	if ok, _ := RefExists(context.Background(), dir, refA); ok {
		t.Fatalf("refA must not exist: partial application of a failed transaction")
	}
}

func TestUpdateRefsTx_RejectsMalformedRefName(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	bad := "refs/no-mistakes/hold/inject\nupdate refs/heads/main " + sha
	if err := UpdateRefsTx(context.Background(), dir, []RefTxOp{{Verb: "update", Ref: bad, NewOID: sha, OldOID: ZeroOID}}); err == nil {
		t.Fatalf("expected malformed ref name to be rejected before any subprocess I/O")
	}
}

func TestWorktreeList_ParsesRegisteredWorktrees(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	wt1 := filepath.Join(t.TempDir(), "wt1")
	wt2 := filepath.Join(t.TempDir(), "wt2")
	if err := WorktreeAdd(context.Background(), dir, wt1, sha); err != nil {
		t.Fatalf("worktree add 1: %v", err)
	}
	if err := WorktreeAdd(context.Background(), dir, wt2, sha); err != nil {
		t.Fatalf("worktree add 2: %v", err)
	}
	entries, err := WorktreeList(context.Background(), dir)
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	seen := map[string]WorktreeEntry{}
	for _, e := range entries {
		seen[e.Path] = e
	}
	for _, wt := range []string{wt1, wt2} {
		resolved, _ := filepath.EvalSymlinks(wt)
		if resolved == "" {
			resolved = wt
		}
		e, ok := seen[filepath.Clean(resolved)]
		if !ok {
			t.Fatalf("missing registered worktree entry for %s in %+v", wt, entries)
		}
		if e.HEAD != sha || !e.Detached {
			t.Fatalf("entry for %s = %+v; want HEAD %s detached", wt, e, sha)
		}
	}
}

func TestWorktreeList_SurvivesDeletedWorktreeDirectory(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	wtParent := t.TempDir()
	wt := filepath.Join(wtParent, "wt")
	if err := WorktreeAdd(context.Background(), dir, wt, sha); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	// Simulate a crash between directory deletion and `worktree prune`: the
	// admin entry is the only thing left, and it must still be observable.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	entries, err := WorktreeList(context.Background(), dir)
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	resolved := filepath.Clean(wt)
	for _, e := range entries {
		if e.Path == resolved {
			return
		}
	}
	t.Fatalf("registration for deleted worktree directory must survive until an explicit prune, got %+v", entries)
}

func TestForEachRefMerged_ReturnsOnlyMergedHolds(t *testing.T) {
	dir, sha := newBareRepoWithCommit(t)
	merged := commitOn(t, dir, sha)
	unmerged := commitOn(t, dir, sha)
	if err := UpdateRefsTx(context.Background(), dir, []RefTxOp{
		{Verb: "update", Ref: "refs/no-mistakes/hold/01MERGEDXXXXXXXXXXXXXXXXXX", NewOID: merged, OldOID: ZeroOID},
		{Verb: "update", Ref: "refs/no-mistakes/hold/01UNMERGEDXXXXXXXXXXXXXXXX", NewOID: unmerged, OldOID: ZeroOID},
	}); err != nil {
		t.Fatalf("seed holds: %v", err)
	}
	got, err := ForEachRefMerged(context.Background(), dir, merged, "refs/no-mistakes/hold/")
	if err != nil {
		t.Fatalf("ForEachRefMerged: %v", err)
	}
	if len(got) != 1 || got[0].Ref != "refs/no-mistakes/hold/01MERGEDXXXXXXXXXXXXXXXXXX" {
		t.Fatalf("ForEachRefMerged = %+v; want exactly the merged hold", got)
	}
}
