// Package custody is the sole owner of the gate-side anchoring and release
// policy: Git remains the only custody authority (a per-run hold ref plus
// the registered-worktree admin entry), and nothing here reads or writes
// SQLite or any non-Git state. Protected(head) = registered-worktree-HEAD OR
// hold-ref, and every destructive worktree removal in this codebase must go
// through AnchorAndRemoveRunWorktree so that disjunction can never be
// bypassed.
package custody

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

const holdPrefix = "refs/no-mistakes/hold/"

// canonicalULID matches a 26-character Crockford Base32 ULID: uppercase
// digits/letters, excluding I, L, O, U (see internal/db.newID). Run IDs are
// the only untrusted input that reaches a ref name in this package, so this
// is the choke point that keeps a malformed or hostile run id from ever
// reaching git.UpdateRefsTx.
var canonicalULID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidateRunID reports whether id is a canonical 26-character ULID.
func ValidateRunID(id string) error {
	if !canonicalULID.MatchString(id) {
		return fmt.Errorf("custody: %q is not a canonical run id", id)
	}
	return nil
}

// HoldRef returns the gate-local hold ref for runID.
func HoldRef(runID string) string {
	return holdPrefix + runID
}

// AnchorCurrent makes currentHead durably protected by runID's gate hold
// ref, independent of the registered worktree admin entry. Absent creates
// the hold; equal is an idempotent no-op; different CASes from the exact
// observed old value, so a legitimate non-fast-forward rewrite of the same
// run's own head (rebase, amend) still succeeds, while a stale concurrent
// writer whose observed old value no longer matches aborts the whole
// transaction and changes nothing - the caller must retry or retain.
func AnchorCurrent(ctx context.Context, gateDir, runID, currentHead string) error {
	if err := ValidateRunID(runID); err != nil {
		return err
	}
	if !git.ValidOID(currentHead) || currentHead == git.ZeroOID {
		return fmt.Errorf("custody: invalid current head %q", currentHead)
	}
	ref := HoldRef(runID)
	oldOID := git.ZeroOID
	if existing, err := git.ResolveRef(ctx, gateDir, ref); err == nil {
		if existing == currentHead {
			return nil
		}
		oldOID = existing
	}
	if err := git.UpdateRefsTx(ctx, gateDir, []git.RefTxOp{
		{Verb: "update", Ref: ref, NewOID: currentHead, OldOID: oldOID},
	}); err != nil {
		return fmt.Errorf("custody: anchor %s: %w", runID, err)
	}
	readback, err := git.ResolveRef(ctx, gateDir, ref)
	if err != nil || readback != currentHead {
		return fmt.Errorf("custody: anchor %s: read-back mismatch after transaction", runID)
	}
	return nil
}

// AnchorAndRemoveRunWorktree is the single guarded choke point every
// production worktree removal must go through. It resolves the worktree's
// current HEAD from Git's own registration (never from a caller-supplied or
// DB-recorded SHA, which could be stale relative to in-worktree commits made
// during the run), anchors that exact head, and only then removes the
// worktree. Any failure - the path is not a registered worktree of this
// gate, runID is not canonical, or the anchor transaction is rejected -
// retains the worktree untouched rather than falling back to a raw
// directory delete.
func AnchorAndRemoveRunWorktree(ctx context.Context, gateDir, wtPath, runID string) error {
	if err := ValidateRunID(runID); err != nil {
		return err
	}
	entries, err := git.WorktreeList(ctx, gateDir)
	if err != nil {
		return fmt.Errorf("custody: list worktrees: %w", err)
	}
	target := filepath.Clean(wtPath)
	if resolved, err := filepath.EvalSymlinks(wtPath); err == nil {
		target = filepath.Clean(resolved)
	}
	var head string
	found := false
	for _, e := range entries {
		if e.Path == target {
			head, found = e.HEAD, true
			break
		}
	}
	if !found {
		return fmt.Errorf("custody: %s is not a registered worktree of the gate", wtPath)
	}
	if err := AnchorCurrent(ctx, gateDir, runID, head); err != nil {
		return fmt.Errorf("custody: anchor before removal: %w", err)
	}
	return git.WorktreeRemove(ctx, gateDir, wtPath)
}

// RetireMergedHolds deletes hold refs under runID namespace whose commit is
// an ancestor of (or equal to) pushedTip, in one transaction that also
// verifies branchRef is still exactly at pushedTip. If the branch has moved
// past pushedTip since the caller observed it, the whole transaction is
// refused and no hold is deleted - a bounded batch is retried on the next
// qualifying push rather than certifying release against a tip this call
// never actually confirmed. Bounded to at most limit refs so one push
// notification never blocks on unbounded prior debt.
func RetireMergedHolds(ctx context.Context, gateDir, branchRef, pushedTip string, limit int) (int, error) {
	merged, err := git.ForEachRefMerged(ctx, gateDir, pushedTip, holdPrefix)
	if err != nil {
		return 0, fmt.Errorf("custody: list merged holds: %w", err)
	}
	if len(merged) == 0 {
		return 0, nil
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	ops := make([]git.RefTxOp, 0, len(merged)+1)
	ops = append(ops, git.RefTxOp{Verb: "verify", Ref: branchRef, OldOID: pushedTip})
	for _, rv := range merged {
		ops = append(ops, git.RefTxOp{Verb: "delete", Ref: rv.Ref, OldOID: rv.OID})
	}
	if err := git.UpdateRefsTx(ctx, gateDir, ops); err != nil {
		return 0, fmt.Errorf("custody: retire merged holds: %w", err)
	}
	return len(merged), nil
}
