package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCIStep_PersistentReaderErrorParksWithEvidence is the red-first
// regression test for a CI step whose reader (GetChecks) keeps erroring: with
// no PR state or mergeability issue known, the only prior way out of the poll
// loop was CITimeout (which can be configured unlimited), so the step never
// parked and axi respond had nothing to act on. It must instead park after a
// small number of consecutive read failures, carrying the reader error as
// ask-user evidence so a reachable skip can record honestly why it was
// skipped.
func TestCIStep_PersistentReaderErrorParksWithEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	const secret = "reader-secret"
	env := fakeCIGHChecksError(t, "OPEN", "", "gh: could not read https://user:"+secret+"@github.com/test/repo checks (exit 1)")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	// Unlimited: PR state is OPEN and mergeability is clean (no known issue),
	// so only the reader-error escalation - not the idle CITimeout - may end
	// this loop. Before the fix this proves the loop never terminates on its
	// own; after the fix it proves the escalation is what ends it.
	sctx.Config.CITimeout = -1

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount > ciReaderErrorParkThreshold+5 {
				return fmt.Errorf("test safety net: polled %d times without parking on the persistent reader error", pollCount)
			}
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected the step to park instead of erroring out, got: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("expected NeedsApproval after repeated CI read failures, got: %+v", outcome)
	}
	if pollCount > ciReaderErrorParkThreshold {
		t.Fatalf("expected the step to park within %d consecutive reader errors, took %d polls", ciReaderErrorParkThreshold, pollCount)
	}

	var findings Findings
	if uerr := json.Unmarshal([]byte(outcome.Findings), &findings); uerr != nil {
		t.Fatalf("unmarshal findings: %v", uerr)
	}
	foundReaderError := false
	for _, item := range findings.Items {
		if item.Action == types.ActionAskUser && strings.Contains(strings.ToLower(item.Description), "could not read") {
			foundReaderError = true
			if strings.Contains(item.Description, secret) {
				t.Fatalf("persisted reader-error evidence leaked a credential: %q", item.Description)
			}
			if !strings.Contains(item.Description, "https://redacted@github.com/test/repo") {
				t.Fatalf("persisted reader-error evidence was not redacted: %q", item.Description)
			}
		}
	}
	if !foundReaderError {
		t.Fatalf("expected an ask-user finding carrying the reader error, got: %+v", findings.Items)
	}

	found := false
	for _, l := range logs {
		if strings.Contains(l, "could not check CI") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'could not check CI' warnings in logs, got: %v", logs)
	}
}
