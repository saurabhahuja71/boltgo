package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
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

func TestDailySessionPersistsFactualResumeSummary(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, ".bolt", "sessions", "latest.json")
	want := &Agent{Cfg: configForTest(workspace), InferenceProfile: "local-coding-reproducible", History: []llm.Message{{Role: llm.RoleSystem, Content: "system"}}}
	want.EnableDailyMode()
	want.RunState.reset("implement and run go test ./...")
	want.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	want.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureCommand, Output: "FAIL: TestCalc"})
	if _, err := want.SaveSessionPath(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Meta SessionMeta `json:"meta"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Meta.Mode != "Daily" || saved.Meta.Workspace != workspace || len(saved.Meta.ChangedFiles) != 1 || len(saved.Meta.OpenFailures) != 1 {
		t.Fatalf("daily metadata incomplete: %+v", saved.Meta)
	}
	if !strings.Contains(saved.Meta.Summary, "command_failed") || saved.Meta.NextAction == "" {
		t.Fatalf("summary is not factual/resumable: %+v", saved.Meta)
	}
}

func configForTest(workspace string) config.Config {
	cfg := config.Default()
	cfg.Workspace = workspace
	return cfg
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

func TestDailyResumeMarksInFlightActionUnknownAndPreservesQueue(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, ".bolt", "sessions", "latest.json")
	want := &Agent{Cfg: configForTest(workspace), History: []llm.Message{{Role: llm.RoleSystem, Content: "system"}}, PendingRequests: []string{"verify"}, PendingApprovals: []string{"write_file {\"path\":\"x\"}"}}
	want.EnableDailyMode()
	want.RunState.reset("edit x")
	want.RunState.ProviderTurnInProgress = true
	want.RunState.ToolInProgress = "write_file"
	want.RunState.ToolArguments = `{"path":"x"}`
	if _, err := want.SaveSessionPath(path); err != nil {
		t.Fatal(err)
	}
	got := &Agent{Cfg: configForTest(workspace)}
	got.EnableDailyMode()
	if err := got.LoadSessionPath(path); err != nil {
		t.Fatal(err)
	}
	if !got.SessionLoaded || !got.RunState.Interrupted || !got.RunState.UnknownToolOutcome || got.RunState.ProviderTurnInProgress {
		t.Fatalf("resume state lost safety boundary: %+v loaded=%v", got.RunState, got.SessionLoaded)
	}
	if len(got.PendingRequests) != 1 || len(got.PendingApprovals) != 1 {
		t.Fatalf("queued facts lost: requests=%v approvals=%v", got.PendingRequests, got.PendingApprovals)
	}
}
