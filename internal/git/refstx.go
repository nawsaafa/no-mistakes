package git

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// ZeroOID is the null object id git uses in an update-ref --stdin old-value
// position to mean "this ref must not currently exist".
const ZeroOID = "0000000000000000000000000000000000000000"

var hexOID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidOID reports whether s is a well-formed 40-hex object id or ZeroOID.
func ValidOID(s string) bool {
	return s == ZeroOID || hexOID.MatchString(s)
}

// refNameSyntax is deliberately conservative rather than a full
// git-check-ref-format implementation: UpdateRefsTx only ever receives refs
// this codebase generates itself (gate branches, refs/no-mistakes/hold/*), so
// the bar is "cannot inject an extra update-ref --stdin line or escape the
// refs/ namespace", not full git ref-name parity.
var refNameSyntax = regexp.MustCompile(`^refs/[!-~]+$`)

func validRefName(ref string) bool {
	return refNameSyntax.MatchString(ref) && !strings.Contains(ref, "..") && !strings.Contains(ref, "//")
}

// RefTxOp is one line of a git update-ref --stdin transaction.
type RefTxOp struct {
	Verb   string // "update", "delete", or "verify"
	Ref    string // full ref name, e.g. refs/no-mistakes/hold/<runID>
	NewOID string // desired value; required for "update"
	OldOID string // required expected old value (ZeroOID means "must be absent")
}

// UpdateRefsTx submits ops as one atomic `git update-ref --stdin` transaction:
// every verify/update/delete either all take effect together or none do. Each
// op's OldOID is the exact expected current value (not merely "is an
// ancestor"), so a stale writer whose observed old value no longer matches
// aborts the whole transaction and leaves every ref exactly as it was - no
// ref is ever partially updated and no fallback to a forced write exists.
func UpdateRefsTx(ctx context.Context, dir string, ops []RefTxOp) error {
	if len(ops) == 0 {
		return fmt.Errorf("update-ref transaction: no operations")
	}
	var b strings.Builder
	b.WriteString("start\n")
	for _, op := range ops {
		if !validRefName(op.Ref) {
			return fmt.Errorf("update-ref transaction: invalid ref name %q", op.Ref)
		}
		if !ValidOID(op.OldOID) {
			return fmt.Errorf("update-ref transaction: invalid old oid for %s", op.Ref)
		}
		switch op.Verb {
		case "update":
			if !ValidOID(op.NewOID) || op.NewOID == ZeroOID {
				return fmt.Errorf("update-ref transaction: invalid new oid for %s", op.Ref)
			}
			fmt.Fprintf(&b, "update %s %s %s\n", op.Ref, op.NewOID, op.OldOID)
		case "delete":
			fmt.Fprintf(&b, "delete %s %s\n", op.Ref, op.OldOID)
		case "verify":
			fmt.Fprintf(&b, "verify %s %s\n", op.Ref, op.OldOID)
		default:
			return fmt.Errorf("update-ref transaction: unknown verb %q", op.Verb)
		}
	}
	b.WriteString("commit\n")
	if _, err := runWithStdin(ctx, dir, b.String(), "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("update-ref transaction: %w", err)
	}
	return nil
}

// RefValue is one ref/OID pair returned by a ref query.
type RefValue struct {
	Ref string
	OID string
}

// ForEachRef lists every ref under prefix.
func ForEachRef(ctx context.Context, dir, prefix string) ([]RefValue, error) {
	return forEachRef(ctx, dir, prefix, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
}

// ForEachRefMerged lists every ref under prefix whose commit is an ancestor
// of (or equal to) mergedInto.
func ForEachRefMerged(ctx context.Context, dir, mergedInto, prefix string) ([]RefValue, error) {
	return forEachRef(ctx, dir, prefix, "for-each-ref", "--format=%(refname) %(objectname)", "--merged="+mergedInto, prefix)
}

func forEachRef(ctx context.Context, dir, prefix string, args ...string) ([]RefValue, error) {
	out, err := Run(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("for-each-ref %s: %w", prefix, err)
	}
	var vals []RefValue
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		vals = append(vals, RefValue{Ref: fields[0], OID: fields[1]})
	}
	return vals, nil
}

func runWithStdin(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = NonInteractiveEnv(dir)
	cmd.Stdin = strings.NewReader(stdin)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(stderr))
	}
	return strings.TrimSpace(string(out)), nil
}
