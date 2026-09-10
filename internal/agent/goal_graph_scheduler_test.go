package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func schedulerGraph(nodes ...GoalNode) GoalGraph {
	return GoalGraph{Version: 1, GoalID: "scheduler", OriginalGoal: "execute graph", Nodes: nodes}
}

func schedulerEvidence(s *GoalGraphScheduler, t *testing.T, kind string) {
	t.Helper()
	if err := s.ObserveCurrent(GoalEvidence{Kind: kind, Source: "test", Summary: "authoritative evidence", Satisfied: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerDependencyOrdering(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true, "a"), graphNode("c", GoalNodeTestExecution, true, "b")))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a", "b", "c"} {
		got, ok, err := s.StartNext()
		if err != nil || !ok || got != want {
			t.Fatalf("StartNext = %q, %v, %v; want %q", got, ok, err, want)
		}
		schedulerEvidence(s, t, "runtime")
	}
	if _, ok, err := s.StartNext(); err != nil || ok {
		t.Fatalf("scheduler executed after terminal completion: ok=%v err=%v", ok, err)
	}
}

func TestSchedulerMultiplePrerequisitesAndIndependentTieBreak(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("b", GoalNodeImplementation, true), graphNode("a", GoalNodeExploration, true), graphNode("c", GoalNodeArtifact, true, "a", "b")))
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := s.Graph.ReadyNodeIDs()
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ready = %v", ids)
	}
	id, _, _ := s.StartNext()
	if id != "a" {
		t.Fatalf("tie break = %q", id)
	}
	schedulerEvidence(s, t, "runtime")
	id, _, _ = s.StartNext()
	if id != "b" {
		t.Fatalf("second tie break = %q", id)
	}
	schedulerEvidence(s, t, "runtime")
	id, _, _ = s.StartNext()
	if id != "c" {
		t.Fatalf("join node = %q", id)
	}
}

func TestSchedulerBlockedAndFailedNodes(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true, "a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if err = s.BlockCurrent(GoalFailure{Source: "test", Summary: "blocked"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.SelectReady(); err != nil || ok {
		t.Fatalf("blocked dependency became ready: %v %v", ok, err)
	}
	if !s.BlockedRequired() {
		t.Fatal("required blocked graph was not reported blocked")
	}
	if err = s.RetryCurrent(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if err = s.FailCurrent(GoalFailure{Source: "test", Summary: "failed", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryCurrent(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerSatisfiedNodesAreNotReexecuted(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, false)))
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := s.StartNext()
	if id != "a" {
		t.Fatal(id)
	}
	schedulerEvidence(s, t, "runtime")
	if s.Graph.Nodes[0].Attempts != 1 {
		t.Fatalf("attempts = %d", s.Graph.Nodes[0].Attempts)
	}
	id, _, _ = s.StartNext()
	if id != "b" {
		t.Fatalf("optional node = %q", id)
	}
	schedulerEvidence(s, t, "runtime")
	if _, ok, _ := s.StartNext(); ok {
		t.Fatal("satisfied graph executed again")
	}
}

func TestSchedulerRejectsModelAndMissingEvidence(t *testing.T) {
	s, err := NewGoalGraphScheduler(validSingleGraph())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveCurrent(GoalEvidence{Kind: "model", Source: "assistant", Satisfied: true}); err == nil {
		t.Fatal("model assertion accepted as evidence")
	}
	if err = s.ObserveCurrent(GoalEvidence{Kind: "runtime", Source: "test", Satisfied: false}); err != nil {
		t.Fatal(err)
	}
	if s.RequiredComplete() {
		t.Fatal("unsatisfied observation completed node")
	}
	if s.Graph.Nodes[0].Status != GoalNodeObserved {
		t.Fatalf("status = %s", s.Graph.Nodes[0].Status)
	}
}

func TestSchedulerTask11IndependentVerification(t *testing.T) {
	s, err := NewGoalGraphScheduler(task11Graph())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"explore-average", "implement-or-confirm-average", "verify-relevant-test"} {
		got, _, e := s.StartNext()
		if e != nil || got != id {
			t.Fatalf("start = %q, %v", got, e)
		}
		kind := "runtime"
		schedulerEvidence(s, t, kind)
	}
	if s.Graph.Nodes[3].Status == GoalNodeSatisfied || s.RequiredComplete() {
		t.Fatal("relevant test satisfied full verification")
	}
	id, ok, err := s.StartNext()
	if err != nil || !ok || id != "verify-go-test-all" {
		t.Fatalf("full test was not next: %q %v %v", id, ok, err)
	}
	schedulerEvidence(s, t, "runtime")
	id, ok, err = s.StartNext()
	if err != nil || !ok || id != "final-verification" {
		t.Fatalf("final verification not eligible: %q %v %v", id, ok, err)
	}
}

func TestSchedulerTask11FullProgressionToDone(t *testing.T) {
	s, err := NewGoalGraphScheduler(task11Graph())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Scheduler: s}
	for _, id := range []string{"explore-average", "implement-or-confirm-average"} {
		if got, ok, e := s.StartNext(); e != nil || !ok || got != id {
			t.Fatalf("start = %q, %v, %v; want %q", got, ok, e, id)
		}
		schedulerEvidence(s, t, "runtime")
	}
	checks := []struct {
		id, args string
	}{
		{"verify-relevant-test", `{"command":"go test ./calc -v"}`},
		{"verify-go-test-all", `{"command":"go test ./..."}`},
		{"final-verification", `{"command":"go test ./..."}`},
	}
	for _, check := range checks {
		if got, ok, e := s.StartNext(); e != nil || !ok || got != check.id {
			t.Fatalf("start = %q, %v, %v; want %q", got, ok, e, check.id)
		}
		a.RunState.recordVerification("run_tests", check.args, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
		a.projectGoalGraphEvidence("run_tests", check.args, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	}
	if !s.RequiredComplete() || s.Graph.Status != GoalGraphComplete {
		t.Fatalf("task 11 graph did not reach DONE: status=%s graph=%#v", s.Graph.Status, s.Graph)
	}
}

func TestSchedulerPreservesEvidenceAndGoal(t *testing.T) {
	g := validSingleGraph()
	g.OriginalGoal = "preserve this exact goal"
	s, err := NewGoalGraphScheduler(g)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.StartNext()
	schedulerEvidence(s, t, "runtime")
	if s.Graph.OriginalGoal != g.OriginalGoal || len(s.Graph.Nodes[0].Evidence) != 1 {
		t.Fatalf("goal/evidence lost: %#v", s.Graph)
	}
	if err = s.Graph.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerBoundsAndRuntimeEvidenceProjection(t *testing.T) {
	s, err := NewGoalGraphScheduler(task11Graph())
	if err != nil {
		t.Fatal(err)
	}
	s.ExecutionAllowed = false
	if _, ok, err := s.StartNext(); err != nil || ok {
		t.Fatalf("bounded scheduler ran: %v %v", ok, err)
	}
	s.ExecutionAllowed = true
	for _, id := range []string{"explore-average", "implement-or-confirm-average"} {
		started, ok, e := s.StartNext()
		if e != nil || !ok || started != id {
			t.Fatalf("start = %q, %v, %v", started, ok, e)
		}
		schedulerEvidence(s, t, "runtime")
	}
	a := &Agent{Scheduler: s}
	a.RunState.VerificationCriteria = []VerificationCriterion{{Key: "run_tests:{\"command\":\"go test ./...\"}", Status: VerificationPassed, LastTool: "run_tests"}}
	if _, _, err = a.startGoalGraphNode(); err != nil {
		t.Fatal(err)
	}
	a.projectGoalGraphEvidence("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if s.Graph.Nodes[2].Status != GoalNodeRunning {
		t.Fatalf("relevant node incorrectly satisfied by full test: %s", s.Graph.Nodes[2].Status)
	}
}

func TestGoalGraphExplorationEvidenceIsPredicateAware(t *testing.T) {
	tests := []struct {
		name string
		node GoalNode
		tool string
		want bool
	}{
		{"repo map repository discovery", GoalNode{Description: "map repository structure", Type: GoalNodeExploration, EvidencePredicate: "repository discovery evidence"}, "repo_map", true},
		{"repo map unrelated deployment", GoalNode{Description: "verify deployment result", Type: GoalNodeExploration, EvidencePredicate: "deployment succeeded"}, "repo_map", false},
		{"list directory filesystem discovery", GoalNode{Description: "inspect workspace files", Type: GoalNodeExploration, EvidencePredicate: "filesystem discovery evidence"}, "list_dir", true},
		{"read source content", GoalNode{Description: "inspect implementation source", Type: GoalNodeExploration, EvidencePredicate: "source content evidence"}, "read_file", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := explorationEvidenceMatches(tt.node, tt.tool); got != tt.want {
				t.Fatalf("explorationEvidenceMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRepoMapProjectsAuthoritativeExplorationEvidence(t *testing.T) {
	graph := schedulerGraph(GoalNode{
		ID: "explore", Description: "discover repository structure", Type: GoalNodeExploration,
		Status: GoalNodePending, Required: true, EvidencePredicate: "repository discovery evidence",
	})
	s, err := NewGoalGraphScheduler(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Scheduler: s}
	a.projectGoalGraphEvidence("repo_map", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "workspace/\n  calc.go"})
	if s.Graph.Nodes[0].Status != GoalNodeSatisfied || len(s.Graph.Nodes[0].Evidence) != 1 {
		t.Fatalf("repo_map evidence did not satisfy exploration node: %#v", s.Graph.Nodes[0])
	}
}

func TestGoalGraphToolScope(t *testing.T) {
	graph := GoalGraph{Version: 1, GoalID: "scope", OriginalGoal: "scope tools", Nodes: []GoalNode{
		{ID: "explore", Description: "explore repository", Type: GoalNodeExploration, Status: GoalNodePending, Required: true, EvidencePredicate: "repository discovery"},
		{ID: "implement", Description: "implement change", Type: GoalNodeImplementation, Status: GoalNodePending, Required: true, DependsOn: []string{"explore"}},
		{ID: "verify", Description: "run tests", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"implement"}, EvidencePredicate: "full go test passed"},
	}}
	s, err := NewGoalGraphScheduler(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"repo_map", "list_dir", "read_file"} {
		if !s.AllowsTool(tool, `{}`) {
			t.Errorf("exploration should allow %s", tool)
		}
	}
	for _, call := range [][2]string{{"str_replace", `{"path":"calc.go"}`}, {"run_tests", `{"command":"go test ./..."}`}} {
		if s.AllowsTool(call[0], call[1]) {
			t.Errorf("exploration should reject %s", call[0])
		}
	}
	if err = s.ObserveCurrent(GoalEvidence{Kind: "runtime", Source: "repo_map", Satisfied: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if !s.AllowsTool("read_file", `{"path":"calc.go"}`) || !s.AllowsTool("str_replace", `{"path":"calc.go"}`) || s.AllowsTool("run_tests", `{}`) {
		t.Fatal("implementation scope was not enforced")
	}
}

func TestGoalGraphTestCreationAllowsOnlyTestFileMutation(t *testing.T) {
	graph := GoalGraph{Version: 1, GoalID: "test-scope", OriginalGoal: "add a test", Nodes: []GoalNode{
		{ID: "tests", Description: "add focused tests", Type: GoalNodeTestCreation, Status: GoalNodePending, Required: true},
	}}
	s, err := NewGoalGraphScheduler(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if !s.AllowsTool("str_replace", `{"path":"calc/calc_test.go"}`) || !s.AllowsTool("write_file", `{"path":"calc/regression_test.go"}`) {
		t.Fatal("test creation did not allow test-file mutation")
	}
	if s.AllowsTool("str_replace", `{"path":"calc/calc.go"}`) || s.AllowsTool("run_tests", `{"command":"go test ./..."}`) {
		t.Fatal("test creation scope allowed implementation or verification work")
	}
}

func TestGoalGraphRoutesUnambiguousReadyTestAction(t *testing.T) {
	g := GoalGraph{Version: 1, GoalID: "route", OriginalGoal: "implement and test", Nodes: []GoalNode{
		{ID: "implement", Description: "implementation is complete", Type: GoalNodeImplementation, Status: GoalNodeSatisfied, Required: true, Evidence: []GoalEvidence{{Kind: "runtime", Source: "test", Summary: "complete", Satisfied: true}}},
		{ID: "tests", Description: "add tests", Type: GoalNodeTestCreation, Status: GoalNodePending, Required: true, DependsOn: []string{"implement"}},
		{ID: "active", Description: "independent inspection", Type: GoalNodeExploration, Status: GoalNodePending, Required: false},
	}}
	s, err := NewGoalGraphScheduler(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	// The unrelated ready node is active while the test node is also ready.
	if s.CurrentNodeID != "active" {
		t.Fatalf("active setup node = %q", s.CurrentNodeID)
	}
	if id, routed, err := s.RouteAction("write_file", `{"path":"calc_test.go"}`); err != nil || !routed || id != "tests" {
		t.Fatalf("route = %q, %v, %v; want tests,true,nil", id, routed, err)
	}
	if s.CurrentNodeID != "tests" || s.Graph.Nodes[s.Graph.nodeIndex()["tests"]].Status != GoalNodeRunning {
		t.Fatalf("test node was not activated: current=%q graph=%#v", s.CurrentNodeID, s.Graph.Nodes)
	}
}

func TestGoalGraphRoutingFailsClosedForAmbiguousAndOutOfScopeActions(t *testing.T) {
	g := GoalGraph{Version: 1, GoalID: "ambiguous", OriginalGoal: "add tests", Nodes: []GoalNode{
		{ID: "current", Description: "artifact", Type: GoalNodeArtifact, Status: GoalNodePending, Required: false},
		{ID: "test-a", Description: "add test a", Type: GoalNodeTestCreation, Status: GoalNodePending, Required: true},
		{ID: "test-b", Description: "add test b", Type: GoalNodeTestCreation, Status: GoalNodePending, Required: true},
	}}
	s, err := NewGoalGraphScheduler(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if _, routed, err := s.RouteAction("write_file", `{"path":"calc_test.go"}`); err != nil || routed {
		t.Fatalf("ambiguous route = %v, %v", routed, err)
	}
	if _, routed, err := s.RouteAction("write_file", `{"path":"calc.go"}`); err != nil || routed {
		t.Fatalf("out-of-scope implementation target was routed: %v, %v", routed, err)
	}
}

func TestGoalGraphImplementationDoesNotAuthorizeTestTarget(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("impl", GoalNodeImplementation, true)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.StartNext(); err != nil {
		t.Fatal(err)
	}
	if s.AllowsTool("str_replace", `{"path":"calc_test.go"}`) {
		t.Fatal("implementation node authorized test-file mutation")
	}
}

func TestAgentDoesNotDispatchOutOfScopeWork(t *testing.T) {
	writer := &scriptedTool{name: "str_replace"}
	reg := tools.NewRegistry()
	reg.Register(writer)
	ag, _, closeServer := testAgent(t, func(int) string {
		return toolSSE("str_replace", `{"path":"calc.go","old_string":"old","new_string":"new"}`)
	}, reg)
	defer closeServer()
	graph := schedulerGraph(GoalNode{ID: "explore", Description: "explore repository", Type: GoalNodeExploration, Status: GoalNodePending, Required: true, EvidencePredicate: "repository discovery"})
	if err := ag.EnableGoalGraph(graph); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunUserMessage(context.Background(), "inspect the repository", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("out-of-scope mutation was dispatched: %v", writer.calls)
	}
	if ag.Scheduler.Graph.Nodes[0].Status == GoalNodeSatisfied {
		t.Fatal("scope rejection optimistically satisfied exploration")
	}
	found := false
	for _, msg := range ag.History {
		if strings.Contains(msg.Content, "goal_graph_scope_violation") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("scope feedback was not returned to the model history")
	}
}

func TestAgentGoalGraphOptInAndLegacyPath(t *testing.T) {
	a := &Agent{}
	if a.Scheduler != nil {
		t.Fatal("scheduler unexpectedly enabled")
	}
	if err := a.EnableGoalGraph(validSingleGraph()); err != nil {
		t.Fatal(err)
	}
	if a.Scheduler == nil {
		t.Fatal("scheduler was not enabled")
	}
	a.DisableGoalGraph()
	if a.Scheduler != nil {
		t.Fatal("scheduler was not disabled")
	}
}

func TestCurrentObjectiveIsMinimal(t *testing.T) {
	s, err := NewGoalGraphScheduler(schedulerGraph(graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true, "a")))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.StartNext()
	objective := s.CurrentObjective()
	if !strings.Contains(objective, "Current objective") || strings.Contains(objective, "Failures") {
		t.Fatalf("unexpected objective: %q", objective)
	}
}

func TestAgentOptInGraphExecutesDependentNodesThroughExistingLoop(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	writer := &scriptedTool{name: "str_replace"}
	tests := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(writer)
	reg.Register(tests)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("read_file", `{"path":"calc.go"}`)
		case 2:
			return toolSSE("str_replace", `{"path":"calc.go","old_string":"old","new_string":"new"}`)
		case 3:
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		default:
			return textSSE("done")
		}
	}, reg)
	defer closeServer()
	graph := schedulerGraph(graphNode("explore", GoalNodeExploration, true), graphNode("implement", GoalNodeImplementation, true, "explore"), GoalNode{ID: "verify", Description: "run the full test suite", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"implement"}, EvidencePredicate: "full go test passed"})
	if err := ag.EnableGoalGraph(graph); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunUserMessage(context.Background(), "inspect calc.go, fix the implementation, and run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Phase != PhaseComplete || !ag.Scheduler.RequiredComplete() {
		t.Fatalf("graph loop did not complete: phase=%s graph=%#v", ag.RunState.Phase, ag.Scheduler.Graph)
	}
	foundObjective := false
	for _, msg := range (*requests)[1].Messages {
		if strings.Contains(msg.Content, "Current objective") {
			foundObjective = true
			break
		}
	}
	if len(*requests) < 3 || !foundObjective {
		t.Fatalf("node objective was not provided: requests=%d", len(*requests))
	}
	if ag.Scheduler.Graph.Nodes[0].Status != GoalNodeSatisfied || ag.Scheduler.Graph.Nodes[1].Status != GoalNodeSatisfied || ag.Scheduler.Graph.Nodes[2].Status != GoalNodeSatisfied {
		t.Fatalf("dependent nodes not satisfied: %#v", ag.Scheduler.Graph.Nodes)
	}
}

func TestAgentAdvancesGraphBetweenCallsInOneAssistantMessage(t *testing.T) {
	writer := &scriptedTool{name: "str_replace"}
	tests := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(writer)
	reg.Register(tests)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSEMultiple(
				llm.ToolCall{ID: "write", Type: "function", Function: llm.FunctionCall{Name: "str_replace", Arguments: `{"path":"calc_test.go","old_string":"old","new_string":"new"}`}},
				llm.ToolCall{ID: "test", Type: "function", Function: llm.FunctionCall{Name: "run_tests", Arguments: `{"command":"go test ./..."}`}},
			)
		}
		return textSSE("verified")
	}, reg)
	defer closeServer()

	graph := schedulerGraph(
		GoalNode{ID: "add-test", Description: "add the focused regression test", Type: GoalNodeTestCreation, Status: GoalNodePending, Required: true, EvidencePredicate: "test file changed"},
		GoalNode{ID: "run-test", Description: "run the full Go test suite", Type: GoalNodeTestExecution, Status: GoalNodePending, Required: true, DependsOn: []string{"add-test"}, EvidencePredicate: "full go test passed"},
	)
	if err := ag.EnableGoalGraph(graph); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunUserMessage(context.Background(), "add the regression test and run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(writer.calls) != 1 || len(tests.calls) != 1 {
		t.Fatalf("valid calls were not dispatched: writes=%d tests=%d", len(writer.calls), len(tests.calls))
	}
	if !ag.Scheduler.RequiredComplete() {
		t.Fatalf("scheduler did not advance between calls: %#v", ag.Scheduler.Graph.Nodes)
	}
}

func TestAgentGraphRespectsIterationBoundWithoutStartingStaleNode(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, _, closeServer := testAgent(t, func(int) string { return toolSSE("read_file", `{"path":"calc.go"}`) }, reg)
	defer closeServer()
	ag.MaxIterations = 1
	if err := ag.EnableGoalGraph(schedulerGraph(graphNode("explore", GoalNodeExploration, true), graphNode("implement", GoalNodeImplementation, true, "explore"))); err != nil {
		t.Fatal(err)
	}
	if err := ag.RunUserMessage(context.Background(), "inspect calc.go and fix the implementation", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Phase != PhaseBlocked || ag.Scheduler.Graph.Nodes[1].Status != GoalNodePending {
		t.Fatalf("bound bypassed graph dependency: phase=%s nodes=%#v", ag.RunState.Phase, ag.Scheduler.Graph.Nodes)
	}
}
