package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_CloseRunningCIRefusesMissingEvidence(t *testing.T) {
	exec := NewExecutor(nil, nil, nil, nil, nil, nil)
	if err := exec.CloseRunningCI(types.ExternalEvidence{}); err == nil {
		t.Fatal("close CI without evidence must be refused")
	} else if !strings.Contains(err.Error(), "requires both") {
		t.Fatalf("missing-evidence refusal = %v, want required-evidence error", err)
	}
}

func TestExecutor_CloseRunningCIRefusesParkedStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"ci-reader","severity":"warning","description":"reader is blind","action":"ask-user"}]}`
	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepCI, findings)}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	evidence := types.ExternalEvidence{What: "green at parked-head", SuppliedBy: "supervisor"}
	if err := exec.CloseRunningCI(evidence); err == nil {
		t.Fatal("close against parked CI must be refused")
	} else if !strings.Contains(err.Error(), "not currently running") {
		t.Fatalf("parked-close refusal = %v, want running-state error", err)
	}
	if err := exec.Respond(types.StepCI, types.ActionSkip, nil); err != nil {
		t.Fatalf("respond skip: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor failed after skipping parked CI: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after skipping parked CI")
	}
}

func TestExecutor_CloseRunningCIRefusesHealthyReader(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{&healthyCIWaitStep{}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusRunning)

	if err := exec.CloseRunningCI(types.ExternalEvidence{What: "trust me", SuppliedBy: "supervisor"}); err == nil {
		t.Fatal("close against a healthy reader must be refused")
	} else if !strings.Contains(err.Error(), "blind") {
		t.Fatalf("healthy-reader refusal = %v, want blindness error", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healthy CI step did not stop after cancellation")
	}
}

func TestExecutor_DiscardsExternalCloseAtCIFixBoundary(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &staleCICloseRoundStep{ready: make(chan struct{}), continueCh: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusRunning)
	select {
	case <-step.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("CI test step did not become ready for external close")
	}
	if err := exec.CloseRunningCI(types.ExternalEvidence{What: "green at old-head", SuppliedBy: "supervisor"}); err != nil {
		t.Fatalf("close running CI: %v", err)
	}
	close(step.continueCh)
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-reader"}); err != nil {
		t.Fatalf("respond fix: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor failed after fresh CI round: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after CI fix round")
	}
	if step.staleConsumed {
		t.Fatal("CI fix round consumed external evidence stranded by the parked round")
	}
}

// staleCICloseRoundStep models the transition that matters here: the first
// round parks while a close request is buffered, then a user fix starts a new
// round. The executor must discard that request at the gate boundary.
type staleCICloseRoundStep struct {
	calls         int
	staleConsumed bool
	ready         chan struct{}
	continueCh    chan struct{}
}

func (s *staleCICloseRoundStep) Name() types.StepName { return types.StepCI }
func (s *staleCICloseRoundStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.calls++
	if s.calls == 1 {
		sctx.SetCIExternalCloseReady(true)
		close(s.ready)
		<-s.continueCh
		return &StepOutcome{
			NeedsApproval: true,
			Findings:      `{"findings":[{"id":"ci-reader","severity":"warning","description":"reader is blind","action":"ask-user"}]}`,
		}, nil
	}
	select {
	case <-sctx.CIExternalClose:
		s.staleConsumed = true
	default:
	}
	return &StepOutcome{}, nil
}

func TestExecutor_CloseRunningCIPersistsEvidence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{&externalEvidenceWaitStep{}}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusRunning)

	evidence := types.ExternalEvidence{What: "GitHub PR #42 checks green at abc123", SuppliedBy: "release supervisor"}
	if err := exec.CloseRunningCI(evidence); err != nil {
		t.Fatalf("close running CI: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor failed after external close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after external close")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("CI step was not completed: steps=%+v err=%v", steps, err)
	}
	findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil || findings.ExternalEvidence == nil {
		t.Fatalf("persisted external evidence missing: findings=%v err=%v", findings, err)
	}
	if *findings.ExternalEvidence != evidence {
		t.Fatalf("persisted evidence=%+v, want %+v", *findings.ExternalEvidence, evidence)
	}
}

type externalEvidenceWaitStep struct{}

func (s *externalEvidenceWaitStep) Name() types.StepName { return types.StepCI }
func (s *externalEvidenceWaitStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if sctx.SetCIExternalCloseReady != nil {
		sctx.SetCIExternalCloseReady(true)
	}
	select {
	case evidence := <-sctx.CIExternalClose:
		findings, err := json.Marshal(types.Findings{Summary: "externally closed", ExternalEvidence: &evidence})
		if err != nil {
			return nil, err
		}
		return &StepOutcome{Findings: string(findings)}, nil
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
}

type healthyCIWaitStep struct{}

func (s *healthyCIWaitStep) Name() types.StepName { return types.StepCI }
func (s *healthyCIWaitStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if sctx.SetCIExternalCloseReady != nil {
		sctx.SetCIExternalCloseReady(false)
	}
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

// TestExecutor_RunningCIStepWithFailingReaderCanBeSkippedWithEvidence proves
// that once a CI step parks after a persistently failing reader (the
// internal/pipeline/steps.CIStep behavior added alongside this test), the
// existing skip response reaches it and records the reader-error text as
// honest evidence - never the false "aborted by user" step failure that was
// previously the only reachable lever while the step never parked.
func TestExecutor_RunningCIStepWithFailingReaderCanBeSkippedWithEvidence(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	readerErr := "gh pr checks: could not read checks (exit 1)"
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
