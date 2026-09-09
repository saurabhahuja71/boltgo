package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

func TestExplicitSessionPathRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bolt", "sessions", "latest.json")
	want := &Agent{History: []llm.Message{{Role: llm.RoleSystem, Content: "system"}, {Role: llm.RoleUser, Content: "new"}}}
	if _, err := want.SaveSessionPath(path); err != nil {
		t.Fatal(err)
	}
	got := &Agent{}
	if err := got.LoadSessionPath(path); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || got.History[1].Content != "new" {
		t.Fatalf("history = %#v", got.History)
	}
}

func TestSessionRoundTripPreservesAgentRunState(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bolt", "sessions", "latest.json")
	want := &Agent{
		History: []llm.Message{{Role: llm.RoleSystem, Content: "system"}, {Role: llm.RoleUser, Content: "finish the fix"}},
		RunState: AgentRunState{
			OriginalGoal:  "finish the fix",
			Plan:          "run regression tests",
			CurrentStep:   "run_tests",
			Phase:         PhaseReplan,
			Verification:  VerificationPending,
			Iterations:    3,
			Retries:       1,
			ToolCallsUsed: 2,
			VerificationCriteria: []VerificationCriterion{{
				Key: "run_tests:{}", Description: "tests/checks ({})", Status: VerificationBlocked,
				LastTool: "run_tests", LastSummary: "FAIL: TestRegression",
			}},
			AcceptanceCriteriaState: []AcceptanceCriterion{
				{Key: "tests_added", Description: "requested tests added/updated", Satisfied: false},
			},
		},
	}
	if _, err := want.SaveSessionPath(path); err != nil {
		t.Fatal(err)
	}
	got := &Agent{}
	if err := got.LoadSessionPath(path); err != nil {
		t.Fatal(err)
	}
	if got.RunState.OriginalGoal != want.RunState.OriginalGoal || got.RunState.Phase != want.RunState.Phase || got.RunState.Iterations != want.RunState.Iterations {
		t.Fatalf("run state was not resumed: got=%+v want=%+v", got.RunState, want.RunState)
	}
	if len(got.RunState.VerificationCriteria) != 1 || got.RunState.VerificationCriteria[0].Status != VerificationBlocked {
		t.Fatalf("failed verification criterion was not resumed: got=%+v", got.RunState.VerificationCriteria)
	}
	if len(got.RunState.AcceptanceCriteriaState) != 1 || got.RunState.AcceptanceCriteriaState[0].Satisfied {
		t.Fatalf("incomplete acceptance criterion was not resumed: got=%+v", got.RunState.AcceptanceCriteriaState)
	}
}

func TestMissingExplicitSessionDoesNotChangeHistory(t *testing.T) {
	got := &Agent{History: []llm.Message{{Role: llm.RoleSystem, Content: "fresh"}}}
	if err := got.LoadSessionPath(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected missing session error")
	}
	if len(got.History) != 1 || got.History[0].Content != "fresh" {
		t.Fatalf("history changed: %#v", got.History)
	}
}

func TestCorruptExplicitSessionReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bolt", "sessions", "latest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"messages":`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := &Agent{History: []llm.Message{{Role: llm.RoleSystem, Content: "fresh"}}}
	if err := got.LoadSessionPath(path); err == nil {
		t.Fatal("expected corrupt session error")
	}
	if len(got.History) != 1 || got.History[0].Content != "fresh" {
		t.Fatalf("history changed after corrupt load: %#v", got.History)
	}
}

func TestWorkspaceSessionPathRejectsSymlinkedStore(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".bolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".bolt", "sessions")); err != nil {
		t.Fatal(err)
	}
	if _, err := WorkspaceSessionPath(workspace, "latest"); err == nil {
		t.Fatal("expected symlinked session store rejection")
	}
}
