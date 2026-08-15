package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_RunningCIStepWithFailingReaderCanBeSkippedWithEvidence proves
// that once a CI step parks after a persistently failing reader (the
// internal/pipeline/steps.CIStep behavior added alongside this test), the
// existing skip response reaches it and records the reader-error text as
// honest evidence - never the false "aborted by user" step failure that was
// previously the only reachable lever while the step never parked.
func TestExecutor_RunningCIStepWithFailingReaderCanBeSkippedWithEvidence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	readerErr := "gh pr view statusCheckRollup: could not read checks (exit 1)"
	findings := types.Findings{
		Summary: "CI status could not be read",
		Items: []types.Finding{{
			Severity:    "warning",
			Description: "could not read CI status after 3 consecutive attempts: " + readerErr,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}

	// Simulates CIStep.Execute after ciReaderErrorParkThreshold consecutive
	// GetChecks failures: NeedsApproval with the reader error as evidence,
	// exactly as a step that is otherwise still "running" a poll loop would
	// now surface it, instead of looping until an unbounded CITimeout.
	step := newApprovalStep(types.StepCI, string(findingsJSON))
	steps := []Step{step}
	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepCI, types.ActionSkip, nil); err != nil {
		t.Fatalf("respond skip: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no run-level error from a skip, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	ciStep := dbSteps[0]

	if ciStep.Status != types.StepStatusSkipped {
		t.Fatalf("expected step status %q, got %q (error=%v)", types.StepStatusSkipped, ciStep.Status, ciStep.Error)
	}
	if ciStep.Error != nil && strings.Contains(*ciStep.Error, "aborted by user") {
		t.Fatalf("skip must never record the abort-style false failure, got error: %q", *ciStep.Error)
	}
	if ciStep.FindingsJSON == nil || !strings.Contains(*ciStep.FindingsJSON, readerErr) {
		t.Fatalf("expected the persisted findings to carry the reader-error evidence, got: %v", ciStep.FindingsJSON)
	}
}
