package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestGoalGraphOperationalContextIsBoundedAndAuthoritative(t *testing.T) {
	s, err := NewGoalGraphScheduler(GoalGraph{Version: 1, GoalID: "context", OriginalGoal: "exact user goal", Nodes: []GoalNode{
		{ID: "explore", Description: "inspect repository", Type: GoalNodeExploration, Required: true, Status: GoalNodePending, EvidencePredicate: "repository discovery"},
		{ID: "test", Description: "run full tests", Type: GoalNodeVerification, Required: true, Status: GoalNodePending, DependsOn: []string{"explore"}, EvidencePredicate: "full go test passed"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Scheduler: s}
	a.RunState.OriginalGoal = "exact user goal"
	a.RunState.VerificationCriteria = []VerificationCriterion{{Description: "go test ./... passed", Status: VerificationPending}}
	a.recordGoalProgress(GoalProgressScope, "use inspect", "run_tests", false, false)
	ctx := a.goalGraphOperationalContext(16, 24, 4)
	for _, want := range []string{"exact user goal", "explore", "current_node_satisfied: false", "Current objective", "legal_capabilities: inspect", "target_restrictions", "next_ready_node", "remaining_requirements", "verification_still_required", "scope_rejection", "authoritative_state_changed", "remaining_budget"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q: %s", want, ctx)
		}
	}
	if len(ctx) > 4000 {
		t.Fatalf("operational context is not bounded: %d bytes", len(ctx))
	}
}

func TestScopeRejectionConsumesBudgetWithoutToolFailure(t *testing.T) {
	s, err := NewGoalGraphScheduler(GoalGraph{Version: 1, GoalID: "scope", OriginalGoal: "inspect", Nodes: []GoalNode{{ID: "explore", Description: "inspect repository", Type: GoalNodeExploration, Required: true, Status: GoalNodePending, EvidencePredicate: "repository discovery"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.StartNext()
	a := &Agent{Scheduler: s}
	feedback := s.ScopeFeedback("run_tests", `{}`)
	a.RunState.addNonExecutionObservation("run_tests", feedback)
	a.recordGoalProgress(GoalProgressScope, feedback, "run_tests", false, false)
	if a.RunState.ToolCallsUsed != 1 || len(a.RunState.Failures) != 0 {
		t.Fatalf("scope rejection accounting incorrect: state=%#v", a.RunState)
	}
	if len(a.GoalProgress) != 1 || a.GoalProgress[0].Class != GoalProgressScope || a.GoalProgress[0].Executed {
		t.Fatalf("scope event incorrect: %#v", a.GoalProgress)
	}
	if s.Graph.Nodes[0].Status == GoalNodeSatisfied {
		t.Fatal("scope rejection satisfied node")
	}
}

func TestRepeatedEquivalentGoalGraphActionIsRejectedBeforeDispatch(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	a, _, closeServer := testAgent(t, func(int) string {
		return toolSSE("read_file", `{"path":"calc.go"}`)
	}, reg)
	defer closeServer()
	graph := GoalGraph{Version: 1, GoalID: "repeat", OriginalGoal: "inspect unrelated evidence", Nodes: []GoalNode{{ID: "explore", Description: "inspect deployment result", Type: GoalNodeExploration, Required: true, Status: GoalNodePending, EvidencePredicate: "deployment succeeded"}}}
	if err := a.EnableGoalGraph(graph); err != nil {
		t.Fatal(err)
	}
	a.MaxToolRounds = 3
	if err := a.RunUserMessage(context.Background(), "inspect the repository", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 1 {
		t.Fatalf("repeated action was dispatched %d times: %v", len(reader.calls), reader.calls)
	}
	found := false
	for _, event := range a.GoalProgress {
		found = found || event.Class == GoalProgressRepeated
	}
	if !found {
		t.Fatalf("repetition was not classified: %#v", a.GoalProgress)
	}
}

func TestRepeatedActionReopensAfterAuthoritativeStateChange(t *testing.T) {
	a := &Agent{lastActionKey: goalActionKey("node", "read_file", `{"path":"x.go"}`), progressRevision: 2, lastActionRevision: 2, lastActionClass: GoalProgressAuthoritative}
	key := goalActionKey("node", "read_file", `{"path":"x.go"}`)
	if key != a.lastActionKey || a.progressRevision != a.lastActionRevision {
		t.Fatal("test setup is not equivalent")
	}
	if !a.isRepeatedGoalAction(key) {
		t.Fatal("unchanged equivalent action was not recognized")
	}
	// A mutation or new evidence increments the authoritative revision. The
	// same read is then eligible again; argument equality alone is insufficient
	// to call it unproductive.
	a.progressRevision++
	if a.isRepeatedGoalAction(key) {
		t.Fatal("action remained blocked after authoritative progress")
	}
}

func TestGoalGraphActionClassificationUsesEvidenceNotClaims(t *testing.T) {
	a := &Agent{}
	before := goalGraphSnapshot{NodeID: "n", Status: GoalNodeRunning}
	if got := a.classifyGoalAction("read_file", tools.ExecutionResult{Category: tools.FailureSuccess, Output: "I completed it"}, before); got != GoalProgressAuthoritative {
		t.Fatalf("successful authoritative observation classified as %s", got)
	}
	if got := a.classifyGoalAction("run_tests", tools.ExecutionResult{Category: tools.FailureCommand, Output: "failed"}, before); got != GoalProgressExecution {
		t.Fatalf("execution failure classified as %s", got)
	}
}
