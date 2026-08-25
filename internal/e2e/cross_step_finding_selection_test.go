//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// crossStepFindingScenario makes the review step park with one review-owned
// finding so a later gate can be answered with an ID the active step does not
// own.
func crossStepFindingScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cross-step-finding-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found an issue"
    structured:
      findings:
        - id: "review-1"
          severity: warning
          file: ".no-mistakes.yaml"
          line: 1
          description: "review-owned finding for the operator to decide"
          action: ask-user
      summary: "one review finding"
      risk_level: medium
      risk_rationale: "needs an operator decision"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      title: "feat: cross step finding selection"
      body: "cross step finding selection journey"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write cross-step scenario: %v", err)
	}
	return path
}

// TestAxiCrossStepFindingSelectionJourney drives the real binary end to end:
// the review gate parks with review-1 and is approved, the test gate then
// parks with test-1, and `axi respond --action fix --findings review-1`
// answered at that test gate must be refused by name and owner instead of
// silently dispatching a fix round. The refusal must leave the gate intact so
// the same command with the step's own finding still works, and the fix round
// must report the count it actually dispatched.
func TestAxiCrossStepFindingSelectionJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: crossStepFindingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	failingTest := filepath.Join(h.BinDir, "nm-cross-step-test-failure")
	script := "#!/bin/sh\nprintf 'cross-step test command failed\\n'\nexit 1\n"
	if err := os.WriteFile(failingTest, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing test command: %v", err)
	}

	const branch = "cross-step-finding-selection"
	config := "allow_repo_commands: true\ncommands:\n  test: nm-cross-step-test-failure\n"
	h.CommitChange(branch, ".no-mistakes.yaml", config, "configure failing test command")
	h.PushToGate(branch)

	reviewRun := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	approveOut, err := h.Run("axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve review gate: %v\n%s", err, approveOut)
	}
	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)

	rejectedOut, err := h.Run("axi", "respond", "--action", "fix", "--findings", "review-1")
	t.Logf("axi respond --action fix --findings review-1 (at the test gate):\n%s", rejectedOut)
	if err == nil {
		t.Fatalf("cross-step fix selection was accepted:\n%s", rejectedOut)
	}
	for _, want := range []string{
		"selected finding IDs do not belong to awaiting test step",
		"review-1 (belongs to review step)",
	} {
		if !strings.Contains(rejectedOut, want) {
			t.Errorf("rejection output missing %q:\n%s", want, rejectedOut)
		}
	}

	statusOut, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("axi status after rejected selection: %v\n%s", err, statusOut)
	}
	t.Logf("axi status after the rejected selection:\n%s", statusOut)
	if !strings.Contains(statusOut, "test-1") {
		t.Errorf("status after rejected selection does not still show the test gate finding:\n%s", statusOut)
	}
	parked := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 10*time.Second)
	if parked.ID != reviewRun.ID {
		t.Fatalf("run changed identity: %s != %s", parked.ID, reviewRun.ID)
	}
	if promptedWithForeignFinding(h, "review-owned finding for the operator to decide") {
		t.Error("a rejected cross-step selection reached a test fix agent")
	}

	acceptedOut, err := h.Run("axi", "respond", "--action", "fix", "--findings", "test-1")
	if err != nil {
		t.Fatalf("same-step fix selection was refused: %v\n%s", err, acceptedOut)
	}
	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusFixReview, 60*time.Second)

	logData := readStepLog(t, h, parked.ID, string(types.StepTest))
	if !strings.Contains(logData, "user-fix round starting after round 1 (1 finding selected)") {
		t.Errorf("test log does not report the dispatched selection count:\n%s", logData)
	}
}

// promptedWithForeignFinding reports whether any test-step fix prompt carried
// the review step's finding text.
func promptedWithForeignFinding(h *Harness, description string) bool {
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Previous test findings to address") && strings.Contains(inv.Prompt, description) {
			return true
		}
	}
	return false
}
