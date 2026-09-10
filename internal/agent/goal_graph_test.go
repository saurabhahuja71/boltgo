package agent

import (
	"reflect"
	"strings"
	"testing"
)

func graphNode(id string, typ GoalNodeType, required bool, deps ...string) GoalNode {
	return GoalNode{ID: id, Description: "complete " + id, Type: typ, Status: GoalNodePending, Required: required, DependsOn: deps}
}

func validSingleGraph() GoalGraph {
	return GoalGraph{Version: 1, GoalID: "g", OriginalGoal: "do work", Nodes: []GoalNode{graphNode("work", GoalNodeImplementation, true)}}
}

func task11Graph() GoalGraph {
	return GoalGraph{
		Version: 1, GoalID: "task-11", OriginalGoal: "fix Average and run both tests",
		Nodes: []GoalNode{
			graphNode("explore-average", GoalNodeExploration, true),
			graphNode("implement-or-confirm-average", GoalNodeImplementation, true, "explore-average"),
			{ID: "verify-relevant-test", Description: "run the relevant test", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"implement-or-confirm-average"}, EvidencePredicate: "matching relevant test passed"},
			{ID: "verify-go-test-all", Description: "run go test ./...", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"verify-relevant-test"}, EvidencePredicate: "full go test passed"},
			{ID: "final-verification", Description: "reconcile final verification", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"verify-go-test-all"}, EvidencePredicate: "all required criteria passed"},
		},
	}
}

func withEvidence(g GoalGraph, id string) GoalGraph {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			g.Nodes[i].Evidence = []GoalEvidence{{Kind: "test", Source: id, Summary: "passed", Satisfied: true}}
		}
	}
	return g
}

func satisfyNode(t *testing.T, g GoalGraph, id string) GoalGraph {
	t.Helper()
	var err error
	if g, err = g.TransitionNode(id, GoalNodeReady); err != nil {
		t.Fatal(err)
	}
	if g, err = g.TransitionNode(id, GoalNodeRunning); err != nil {
		t.Fatal(err)
	}
	if g, err = g.TransitionNode(id, GoalNodeObserved); err != nil {
		t.Fatal(err)
	}
	g = withEvidence(g, id)
	if g, err = g.TransitionNode(id, GoalNodeSatisfied); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGoalGraphValidationBasics(t *testing.T) {
	tests := []struct {
		name    string
		graph   GoalGraph
		wantErr string
	}{
		{"empty graph", GoalGraph{}, ""},
		{"single node", validSingleGraph(), ""},
		{"sequential graph", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true, "a"), graphNode("c", GoalNodeTestExecution, true, "b")}}, ""},
		{"multiple prerequisites", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeExploration, true), graphNode("b", GoalNodeExploration, true), graphNode("c", GoalNodeImplementation, true, "a", "b")}}, ""},
		{"independent roots", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true)}}, ""},
		{"unknown dependency", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true, "missing")}}, "unknown dependency"},
		{"self dependency", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true, "a")}}, "depends on itself"},
		{"cycle", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true, "b"), graphNode("b", GoalNodeArtifact, true, "a")}}, "dependency cycle"},
		{"duplicate IDs", GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true), graphNode("a", GoalNodeArtifact, true)}}, "duplicate node ID"},
		{"meaningless description", GoalGraph{Nodes: []GoalNode{{ID: "a", Description: "---", Type: GoalNodeImplementation, Status: GoalNodePending, Required: true}}}, "meaningless description"},
		{"unreachable required node", GoalGraph{RootIDs: []string{"a"}, Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true)}}, "unreachable"},
		{"verification disconnected", GoalGraph{Nodes: []GoalNode{{ID: "verify", Description: "verify", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, EvidencePredicate: "test passed"}}}, "bypasses"},
		{"verification no predicate", GoalGraph{Nodes: []GoalNode{{ID: "verify", Description: "verify", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, DependsOn: []string{"work"}}, graphNode("work", GoalNodeImplementation, true)}}, "evidence predicate"},
		{"required requirement dropped", GoalGraph{RequiredNodeIDs: []string{"missing"}, Nodes: []GoalNode{graphNode("work", GoalNodeImplementation, true)}}, "not represented"},
		{"explicit terminal bypass", GoalGraph{TerminalIDs: []string{"good", "done"}, Nodes: []GoalNode{graphNode("work", GoalNodeImplementation, true), graphNode("good", GoalNodeArtifact, true, "work"), {ID: "done", Description: "finish verification", Type: GoalNodeVerification, Status: GoalNodePending, Required: true, EvidencePredicate: "all checks passed"}}}, "bypasses"},
		{"required node no terminal path", GoalGraph{TerminalIDs: []string{"done"}, Nodes: []GoalNode{graphNode("work", GoalNodeImplementation, true), graphNode("done", GoalNodeArtifact, true, "work"), graphNode("orphan", GoalNodeImplementation, true)}}, "no terminal path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.graph.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestGoalGraphValidationDeterministicAndPure(t *testing.T) {
	g := task11Graph()
	before := g
	for i := 0; i < 5; i++ {
		if err := g.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(g, before) {
		t.Fatalf("Validate mutated graph: before=%#v after=%#v", before, g)
	}
	before = g
	if _, err := g.ReadyNodeIDs(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, before) {
		t.Fatal("readiness mutated graph")
	}
}

func TestGoalGraphReadinessPropagation(t *testing.T) {
	g := task11Graph()
	ready, err := g.ReadyNodeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, []string{"explore-average"}) {
		t.Fatalf("initial ready = %v", ready)
	}
	g = satisfyNode(t, g, "explore-average")
	ready, _ = g.ReadyNodeIDs()
	if !reflect.DeepEqual(ready, []string{"implement-or-confirm-average"}) {
		t.Fatalf("after explore ready = %v", ready)
	}
	g = satisfyNode(t, g, "implement-or-confirm-average")
	ready, _ = g.ReadyNodeIDs()
	if !reflect.DeepEqual(ready, []string{"verify-relevant-test"}) {
		t.Fatalf("after implementation ready = %v", ready)
	}
	if ready, _ = g.ReadyNodeIDsWithOptions(ReadinessOptions{ExecutionAllowed: false}); len(ready) != 0 {
		t.Fatalf("bounded graph was executable: %v", ready)
	}
}

func TestGoalGraphTask11IndependentVerification(t *testing.T) {
	g := task11Graph()
	g = satisfyNode(t, g, "explore-average")
	g = satisfyNode(t, g, "implement-or-confirm-average")
	g = satisfyNode(t, g, "verify-relevant-test")
	ready, err := g.ReadyNodeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, []string{"verify-go-test-all"}) {
		t.Fatalf("only relevant test satisfied, ready = %v", ready)
	}
	if g.Nodes[3].Status != GoalNodePending {
		t.Fatalf("full test was not pending: %s", g.Nodes[3].Status)
	}
	g = task11Graph()
	g = satisfyNode(t, g, "explore-average")
	g = satisfyNode(t, g, "implement-or-confirm-average")
	g = satisfyNode(t, g, "verify-relevant-test")
	g = satisfyNode(t, g, "verify-go-test-all")
	ready, err = g.ReadyNodeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, []string{"final-verification"}) {
		t.Fatalf("both tests satisfied, final ready = %v", ready)
	}
}

func TestGoalGraphBlockedDependencyAndOptionalNode(t *testing.T) {
	g := GoalGraph{Nodes: []GoalNode{graphNode("required", GoalNodeImplementation, true), graphNode("optional", GoalNodeArtifact, false)}}
	ready, err := g.ReadyNodeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ready, []string{"optional", "required"}) {
		t.Fatalf("ready = %v", ready)
	}
	var next GoalGraph
	next, err = g.TransitionNode("optional", GoalNodeReady)
	if err != nil {
		t.Fatal(err)
	}
	next, _ = next.TransitionNode("optional", GoalNodeRunning)
	next, _ = next.TransitionNode("optional", GoalNodeBlocked)
	if ready, _ = next.ReadyNodeIDs(); !reflect.DeepEqual(ready, []string{"required"}) {
		t.Fatalf("blocked optional prevented required node: %v", ready)
	}
	dependent := GoalGraph{Nodes: []GoalNode{graphNode("a", GoalNodeImplementation, true), graphNode("b", GoalNodeArtifact, true, "a")}}
	dependent, _ = dependent.TransitionNode("a", GoalNodeReady)
	dependent, _ = dependent.TransitionNode("a", GoalNodeRunning)
	dependent, _ = dependent.TransitionNode("a", GoalNodeBlocked)
	if ready, _ = dependent.ReadyNodeIDs(); len(ready) != 0 {
		t.Fatalf("blocked dependency allowed dependent: %v", ready)
	}
}

func TestGoalGraphTransitions(t *testing.T) {
	g := validSingleGraph()
	if _, err := g.TransitionNode("work", GoalNodeRunning); err == nil {
		t.Fatal("pending -> running accepted")
	}
	var err error
	if g, err = g.TransitionNode("work", GoalNodeReady); err != nil {
		t.Fatal(err)
	}
	if g, err = g.TransitionNode("work", GoalNodeRunning); err != nil {
		t.Fatal(err)
	}
	if g, err = g.TransitionNode("work", GoalNodeObserved); err != nil {
		t.Fatal(err)
	}
	if _, err = g.TransitionNode("work", GoalNodeSatisfied); err == nil {
		t.Fatal("model-only observed state became satisfied")
	}
	if _, err = g.TransitionNode("work", GoalNodeReady); err == nil {
		t.Fatal("observed -> ready accepted")
	}
	g = withEvidence(g, "work")
	if g, err = g.TransitionNode("work", GoalNodeSatisfied); err != nil {
		t.Fatal(err)
	}
	if _, err = g.TransitionNode("work", GoalNodeFailed); err == nil {
		t.Fatal("satisfied node regressed")
	}
	if _, err = g.TransitionNode("work", GoalNodeReady); err == nil {
		t.Fatal("satisfied node became ready")
	}
	failedTransition := validSingleGraph()
	failedTransition, _ = failedTransition.TransitionNode("work", GoalNodeReady)
	if _, err = failedTransition.TransitionNode("work", GoalNodeObserved); err == nil {
		t.Fatal("ready -> observed accepted")
	}
	if _, err = failedTransition.TransitionNode("work", GoalNodeSatisfied); err == nil {
		t.Fatal("ready -> satisfied accepted")
	}

	failed := validSingleGraph()
	failed, _ = failed.TransitionNode("work", GoalNodeReady)
	failed, _ = failed.TransitionNode("work", GoalNodeRunning)
	failed, _ = failed.TransitionNode("work", GoalNodeFailed)
	failed, err = failed.TransitionNode("work", GoalNodeReady)
	if err != nil {
		t.Fatal(err)
	}
	blocked := validSingleGraph()
	blocked, _ = blocked.TransitionNode("work", GoalNodeReady)
	blocked, _ = blocked.TransitionNode("work", GoalNodeRunning)
	blocked, _ = blocked.TransitionNode("work", GoalNodeBlocked)
	if _, err = blocked.TransitionNode("work", GoalNodeReady); err != nil {
		t.Fatal(err)
	}
}

func TestGoalGraphTerminalPathsAndRequiredNodes(t *testing.T) {
	g := GoalGraph{TerminalIDs: []string{"end-a", "end-b"}, Nodes: []GoalNode{graphNode("start", GoalNodeImplementation, true), graphNode("end-a", GoalNodeArtifact, true, "start"), graphNode("end-b", GoalNodeArtifact, true, "start")}}
	if err := g.Validate(); err != nil {
		t.Fatalf("multiple terminal paths rejected: %v", err)
	}
	if err := (GoalGraph{RequiredNodeIDs: []string{"work"}, Nodes: []GoalNode{graphNode("work", GoalNodeImplementation, true)}}).Validate(); err != nil {
		t.Fatal(err)
	}
}
