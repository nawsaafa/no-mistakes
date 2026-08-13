package custody

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// Deterministic corpus seeds committed for reproducibility: any failure in a
// seeded test below prints this exact seed, so the failing interleaving can
// be replayed byte-for-byte.
const (
	seedConcurrentAnchor = 0xC0570D1A
	seedBoundedBatch     = 0xE1EC7A0D
	seedGCReachability   = 0x0A11CE55
)

func newGateWithCommit(t *testing.T) (gateDir, sha string) {
	t.Helper()
	gateDir = t.TempDir()
	if err := git.InitBare(context.Background(), gateDir); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := git.Run(context.Background(), work, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	if _, err := git.Run(context.Background(), work, "-c", "init.defaultBranch=main", "init"); err != nil {
		t.Fatalf("init work: %v", err)
	}
	run("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "seed")
	sha = strings.TrimSpace(run("rev-parse", "HEAD"))
	run("push", gateDir, "HEAD:refs/heads/main")
	return gateDir, sha
}

func commitOn(t *testing.T, dir, parentSHA, msg string) string {
	t.Helper()
	sha, err := git.Run(context.Background(), dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit-tree", "-p", parentSHA, "-m", msg, parentSHA+"^{tree}")
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	return strings.TrimSpace(sha)
}

func TestValidateRunID(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"", false},
		{"01arz3ndektsv4rrffq69g5fav", false},              // lowercase
		{"01ARZ3NDEKTSV4RRFFQ69G5FA", false},               // too short
		{"01ARZ3NDEKTSV4RRFFQ69G5FAVX", false},             // too long
		{"01ILOU3NDEKTSV4RRFFQ69G5FA", false},              // excluded Crockford letters
		{"../../../../etc/passwd/aaaaaaaaaaaaaaaa", false}, // path traversal attempt
	}
	for _, c := range cases {
		err := ValidateRunID(c.id)
		if (err == nil) != c.valid {
			t.Errorf("ValidateRunID(%q) error = %v, want valid=%v", c.id, err, c.valid)
		}
	}
}

func TestHoldRef_Namespaced(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if got, want := HoldRef(id), "refs/no-mistakes/hold/"+id; got != want {
		t.Fatalf("HoldRef = %q, want %q", got, want)
	}
}

func TestAnchorCurrent_CreatesHoldWhenAbsent(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := AnchorCurrent(context.Background(), gateDir, runID, sha); err != nil {
		t.Fatalf("AnchorCurrent: %v", err)
	}
	got, err := git.ResolveRef(context.Background(), gateDir, HoldRef(runID))
	if err != nil || got != sha {
		t.Fatalf("hold ref = %q, %v; want %s", got, err, sha)
	}
}

func TestAnchorCurrent_IdempotentWhenEqual(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := AnchorCurrent(context.Background(), gateDir, runID, sha); err != nil {
		t.Fatalf("first anchor: %v", err)
	}
	if err := AnchorCurrent(context.Background(), gateDir, runID, sha); err != nil {
		t.Fatalf("second anchor (equal, must be a no-op): %v", err)
	}
}

func TestAnchorCurrent_AllowsNonFastForwardRewriteOfSameRun(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if err := AnchorCurrent(context.Background(), gateDir, runID, sha); err != nil {
		t.Fatalf("first anchor: %v", err)
	}
	// A rebase/amend of the same run's worktree produces a commit that is not
	// a descendant of the previous head. The custody CAS is old-OID-based,
	// not ancestor-based, so this must still succeed.
	rewritten := commitOn(t, gateDir, sha, "rewritten")
	if err := AnchorCurrent(context.Background(), gateDir, runID, rewritten); err != nil {
		t.Fatalf("non-fast-forward re-anchor of the same run must succeed: %v", err)
	}
	got, err := git.ResolveRef(context.Background(), gateDir, HoldRef(runID))
	if err != nil || got != rewritten {
		t.Fatalf("hold ref = %q, %v; want %s", got, err, rewritten)
	}
}

func TestAnchorCurrent_ConcurrentWritersNeverCorruptRef(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	rng := rand.New(rand.NewPCG(seedConcurrentAnchor, seedConcurrentAnchor))
	const n = 12
	heads := make([]string, n)
	for i := range heads {
		heads[i] = commitOn(t, gateDir, sha, fmt.Sprintf("candidate-%d", i))
	}
	rng.Shuffle(n, func(i, j int) { heads[i], heads[j] = heads[j], heads[i] })

	var wg sync.WaitGroup
	var succeeded atomic.Int64
	trace := make([]string, n)
	for i, head := range heads {
		wg.Add(1)
		go func(i int, head string) {
			defer wg.Done()
			err := AnchorCurrent(context.Background(), gateDir, runID, head)
			if err == nil {
				succeeded.Add(1)
				trace[i] = "ok:" + head
			} else {
				trace[i] = "retry-needed:" + head
			}
		}(i, head)
	}
	wg.Wait()

	final, err := git.ResolveRef(context.Background(), gateDir, HoldRef(runID))
	if err != nil {
		t.Fatalf("seed=0x%x trace=%v: hold ref missing after concurrent writers: %v", seedConcurrentAnchor, trace, err)
	}
	valid := false
	for _, h := range heads {
		if h == final {
			valid = true
		}
	}
	if !valid {
		t.Fatalf("seed=0x%x trace=%v: hold ref %s is not one of the attempted heads - corrupted", seedConcurrentAnchor, trace, final)
	}
	if succeeded.Load() == 0 {
		t.Fatalf("seed=0x%x trace=%v: no writer ever succeeded, even with retries expected to converge", seedConcurrentAnchor, trace)
	}
}

func TestAnchorAndRemoveRunWorktree_HappyPath(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	wt := filepath.Join(t.TempDir(), runID)
	if err := git.WorktreeAdd(context.Background(), gateDir, wt, sha); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := AnchorAndRemoveRunWorktree(context.Background(), gateDir, wt, runID); err != nil {
		t.Fatalf("AnchorAndRemoveRunWorktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree directory should be gone, stat err = %v", err)
	}
	got, err := git.ResolveRef(context.Background(), gateDir, HoldRef(runID))
	if err != nil || got != sha {
		t.Fatalf("hold ref after removal = %q, %v; want %s", got, err, sha)
	}
}

func TestAnchorAndRemoveRunWorktree_RefusesUnregisteredPath(t *testing.T) {
	gateDir, _ := newGateWithCommit(t)
	const runID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	unregistered := filepath.Join(t.TempDir(), runID)
	if err := os.MkdirAll(unregistered, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := AnchorAndRemoveRunWorktree(context.Background(), gateDir, unregistered, runID); err == nil {
		t.Fatalf("expected refusal for a path git never registered as a worktree")
	}
	if _, err := os.Stat(unregistered); err != nil {
		t.Fatalf("unregistered directory must be left untouched on refusal: %v", err)
	}
}

func TestAnchorAndRemoveRunWorktree_RejectsNonCanonicalRunID(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	badID := "not-a-ulid"
	wt := filepath.Join(t.TempDir(), badID)
	if err := git.WorktreeAdd(context.Background(), gateDir, wt, sha); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := AnchorAndRemoveRunWorktree(context.Background(), gateDir, wt, badID); err == nil {
		t.Fatalf("expected refusal for a non-canonical run id")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be left untouched on refusal: %v", err)
	}
}

func TestRetireMergedHolds_DeletesOnlyMergedAndVerifiesBranchTip(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	merged := commitOn(t, gateDir, sha, "merged")
	unmerged := commitOn(t, gateDir, sha, "unmerged")
	mergedRunID := "01MERGEDXXXXXXXXXXXXXXXXXX"
	unmergedRunID := "01SEPARATEXXXXXXXXXXXXXXXX"
	if err := AnchorCurrent(context.Background(), gateDir, mergedRunID, merged); err != nil {
		t.Fatalf("anchor merged: %v", err)
	}
	if err := AnchorCurrent(context.Background(), gateDir, unmergedRunID, unmerged); err != nil {
		t.Fatalf("anchor unmerged: %v", err)
	}
	// Advance the branch to a tip that contains `merged` but not `unmerged`.
	tip := commitOn(t, gateDir, merged, "tip")
	if err := git.UpdateRefsTx(context.Background(), gateDir, []git.RefTxOp{{Verb: "update", Ref: "refs/heads/main", NewOID: tip, OldOID: sha}}); err != nil {
		t.Fatalf("advance branch: %v", err)
	}

	n, err := RetireMergedHolds(context.Background(), gateDir, "refs/heads/main", tip, 200)
	if err != nil {
		t.Fatalf("RetireMergedHolds: %v", err)
	}
	if n != 1 {
		t.Fatalf("retired %d holds, want 1", n)
	}
	if ok, _ := git.RefExists(context.Background(), gateDir, HoldRef(mergedRunID)); ok {
		t.Fatalf("merged hold should have been retired")
	}
	if ok, _ := git.RefExists(context.Background(), gateDir, HoldRef(unmergedRunID)); !ok {
		t.Fatalf("unmerged hold must remain")
	}
}

func TestRetireMergedHolds_RefusesWhenBranchMovedPastVerifiedTip(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	merged := commitOn(t, gateDir, sha, "merged")
	runID := "01MERGEDXXXXXXXXXXXXXXXXXX"
	if err := AnchorCurrent(context.Background(), gateDir, runID, merged); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	tip := commitOn(t, gateDir, merged, "tip")
	if err := git.UpdateRefsTx(context.Background(), gateDir, []git.RefTxOp{{Verb: "update", Ref: "refs/heads/main", NewOID: tip, OldOID: sha}}); err != nil {
		t.Fatalf("advance branch: %v", err)
	}
	// A second push races ahead of this retirement call's notion of the tip.
	furtherTip := commitOn(t, gateDir, tip, "further")
	if err := git.UpdateRefsTx(context.Background(), gateDir, []git.RefTxOp{{Verb: "update", Ref: "refs/heads/main", NewOID: furtherTip, OldOID: tip}}); err != nil {
		t.Fatalf("advance branch again: %v", err)
	}

	if _, err := RetireMergedHolds(context.Background(), gateDir, "refs/heads/main", tip, 200); err == nil {
		t.Fatalf("expected retirement to refuse: branch no longer at the verified tip")
	}
	if ok, _ := git.RefExists(context.Background(), gateDir, HoldRef(runID)); !ok {
		t.Fatalf("hold must remain when the verify guard rejects the transaction")
	}
}

func TestRetireMergedHolds_BoundedBatch(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	rng := rand.New(rand.NewPCG(seedBoundedBatch, seedBoundedBatch))
	const total = 9
	const limit = 4
	tip := sha
	var runIDs []string
	for i := 0; i < total; i++ {
		tip = commitOn(t, gateDir, tip, fmt.Sprintf("m%d", i))
		id := fmt.Sprintf("01CAPPED%018d", rng.IntN(1_000_000_000_000_000_000))
		if err := AnchorCurrent(context.Background(), gateDir, id, tip); err != nil {
			t.Fatalf("seed=0x%x anchor %d: %v", seedBoundedBatch, i, err)
		}
		runIDs = append(runIDs, id)
	}
	if err := git.UpdateRefsTx(context.Background(), gateDir, []git.RefTxOp{{Verb: "update", Ref: "refs/heads/main", NewOID: tip, OldOID: sha}}); err != nil {
		t.Fatalf("advance branch: %v", err)
	}

	n, err := RetireMergedHolds(context.Background(), gateDir, "refs/heads/main", tip, limit)
	if err != nil {
		t.Fatalf("seed=0x%x RetireMergedHolds: %v", seedBoundedBatch, err)
	}
	if n != limit {
		t.Fatalf("seed=0x%x retired %d, want exactly the bound %d", seedBoundedBatch, n, limit)
	}
	remaining := 0
	for _, id := range runIDs {
		if ok, _ := git.RefExists(context.Background(), gateDir, HoldRef(id)); ok {
			remaining++
		}
	}
	if remaining != total-limit {
		t.Fatalf("seed=0x%x remaining holds = %d, want %d", seedBoundedBatch, remaining, total-limit)
	}
}

// TestAnchorCurrent_HoldRefPreventsGCReclamation is the RC-2 regression this
// whole package exists to close: object presence (`cat-file -e`) is not
// durability - only reachability from a ref is. An anchored commit that is
// not on any branch must survive `git gc --prune=now`; an unanchored one is
// expected to be reclaimed, which is the negative control proving the test
// actually exercises GC rather than a no-op.
func TestAnchorCurrent_HoldRefPreventsGCReclamation(t *testing.T) {
	gateDir, sha := newGateWithCommit(t)
	rng := rand.New(rand.NewPCG(seedGCReachability, seedGCReachability))
	orphanAnchored := commitOn(t, gateDir, sha, fmt.Sprintf("orphan-anchored-%d", rng.Int64()))
	orphanUnanchored := commitOn(t, gateDir, sha, fmt.Sprintf("orphan-unanchored-%d", rng.Int64()))

	const runID = "01GCPREFXXXXXXXXXXXXXXXXXX"
	if err := AnchorCurrent(context.Background(), gateDir, runID, orphanAnchored); err != nil {
		t.Fatalf("seed=0x%x anchor: %v", seedGCReachability, err)
	}

	if _, err := git.Run(context.Background(), gateDir, "reflog", "expire", "--expire=now", "--all"); err != nil {
		t.Fatalf("reflog expire: %v", err)
	}
	if _, err := git.Run(context.Background(), gateDir, "gc", "--prune=now"); err != nil {
		t.Fatalf("gc: %v", err)
	}

	if ok, _ := git.RefExists(context.Background(), gateDir, orphanAnchored); !ok {
		t.Fatalf("seed=0x%x anchored orphan commit %s was reclaimed by gc - the hold ref did not protect it", seedGCReachability, orphanAnchored)
	}
	if ok, _ := git.RefExists(context.Background(), gateDir, orphanUnanchored); ok {
		t.Fatalf("seed=0x%x negative control failed: unanchored orphan %s survived gc, so this test does not actually exercise reclamation", seedGCReachability, orphanUnanchored)
	}
}
