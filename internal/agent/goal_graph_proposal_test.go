package agent

import (
	"reflect"
	"strings"
	"testing"
)

func proposalForRequirements(reqs []GoalRequirement) GoalGraphProposal {
	nodes := make([]GoalNode, len(reqs))
	previous := ""
	for i, req := range reqs {
		nodes[i] = GoalNode{ID: "node-" + req.ID, Description: req.Description, Type: req.Type, Status: GoalNodePending, Required: req.Required, RequirementIDs: []string{req.ID}, EvidencePredicate: req.EvidencePredicate}
		if previous != "" {
			nodes[i].DependsOn = []string{previous}
		}
		previous = nodes[i].ID
	}
	return GoalGraphProposal{Graph: GoalGraph{Version: 1, GoalID: "proposal", Nodes: nodes}}
}

func TestReconcileSimpleAndTask11Requirements(t *testing.T) {
	tests := []struct {
		name, goal string
		want       int
	}{
		{"simple implementation", "Fix the parser bug.", 1},
		{"task 11", "Inspect Average, fix Average(-4, -6), run the relevant test, then run go test ./....", 4},
		{"artifact", "Fix the parser bug and create REPORT.md documenting the change.", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := ExtractExplicitRequirements(tt.goal)
			got, err := ReconcileGoalGraph(tt.goal, reqs, proposalForRequirements(reqs))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Nodes) != tt.want {
				t.Fatalf("nodes = %d, want %d", len(got.Nodes), tt.want)
			}
			for _, node := range got.Nodes {
				if node.Status != GoalNodePending || len(node.RequirementIDs) != 1 {
					t.Fatalf("unsafe node state: %#v", node)
				}
			}
		})
	}
}

func TestReconcileTask11KeepsIndependentRequirements(t *testing.T) {
	reqs := ExtractExplicitRequirements("Inspect Average, fix Average(-4, -6), run the relevant test, then run go test ./....")
	got, err := ReconcileGoalGraph("task 11", reqs, proposalForRequirements(reqs))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredNodeIDs) != 4 {
		t.Fatalf("required IDs = %v", got.RequiredNodeIDs)
	}
	for _, req := range reqs {
		found := false
		for _, node := range got.Nodes {
			if reflect.DeepEqual(node.RequirementIDs, []string{req.ID}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("requirement %q was collapsed or dropped", req.ID)
		}
	}
}

func TestReconcileRejectsUnsafeProposals(t *testing.T) {
	reqs := ExtractExplicitRequirements("Fix the parser bug and run go test ./....")
	tests := []struct {
		name   string
		mutate func(*GoalGraph)
		want   string
	}{
		{"drops explicit requirement", func(g *GoalGraph) { g.Nodes = g.Nodes[:1] }, "not mapped"},
		{"unknown dependency", func(g *GoalGraph) { g.Nodes[0].DependsOn = []string{"missing"} }, "unknown dependency"},
		{"duplicate IDs", func(g *GoalGraph) { g.Nodes = append(g.Nodes, g.Nodes[0]) }, "duplicate node ID"},
		{"unsupported required work", func(g *GoalGraph) {
			g.Nodes = append(g.Nodes, GoalNode{ID: "invented", Description: "deploy unrelated production system", Type: GoalNodeImplementation, Status: GoalNodePending, Required: true})
		}, "unsupported"},
		{"missing verification predicate", func(g *GoalGraph) {
			for i := range g.Nodes {
				if g.Nodes[i].Type == GoalNodeVerification {
					g.Nodes[i].EvidencePredicate = ""
				}
			}
		}, "evidence predicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := proposalForRequirements(reqs)
			tt.mutate(&p.Graph)
			if _, err := ReconcileGoalGraph("Fix the parser bug and run go test ./....", reqs, p); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReconcileRejectsClaimedSatisfiedNode(t *testing.T) {
	reqs := ExtractExplicitRequirements("Fix the parser bug.")
	p := proposalForRequirements(reqs)
	p.Graph.Nodes[0].Status = GoalNodeSatisfied
	p.Graph.Nodes[0].Evidence = []GoalEvidence{{Kind: "model", Source: "assistant", Satisfied: true}}
	got, err := ReconcileGoalGraph("Fix the parser bug.", reqs, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes[0].Status != GoalNodePending || got.Nodes[0].Evidence[0].Satisfied {
		t.Fatalf("model assertion became evidence: %#v", got.Nodes[0])
	}
}

func TestReconcileAllowsSemanticallyTraceableWording(t *testing.T) {
	reqs := ExtractExplicitRequirements("Fix the parser bug.")
	p := GoalGraphProposal{Graph: GoalGraph{Nodes: []GoalNode{{ID: "repair-parser", Description: "repair parser implementation", Type: GoalNodeImplementation, Status: GoalNodePending, Required: true}}}}
	if _, err := ReconcileGoalGraph("Fix the parser bug.", reqs, p); err != nil {
		t.Fatalf("traceable wording rejected: %v", err)
	}
}

func TestReconcileDeterministicAndInputImmutable(t *testing.T) {
	goal := "Inspect Average, fix it, then run the relevant test and go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	p := proposalForRequirements(reqs)
	beforeReqs := append([]GoalRequirement(nil), reqs...)
	before := p
	a, err := ReconcileGoalGraph(goal, reqs, p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReconcileGoalGraph(goal, reqs, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("reconciliation is not deterministic: %#v != %#v", a, b)
	}
	if !reflect.DeepEqual(reqs, beforeReqs) || !reflect.DeepEqual(p, before) {
		t.Fatal("reconciliation mutated its inputs")
	}
}

func TestReconcileOptionalNodeDoesNotBecomeRequired(t *testing.T) {
	reqs := ExtractExplicitRequirements("Fix the parser bug.")
	p := proposalForRequirements(reqs)
	p.Graph.Nodes = append(p.Graph.Nodes, GoalNode{ID: "optional-note", Description: "optional note", Type: GoalNodeArtifact, Status: GoalNodeSatisfied, Required: false, Evidence: []GoalEvidence{{Kind: "model", Satisfied: true}}})
	got, err := ReconcileGoalGraph("Fix the parser bug.", reqs, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes[1].Required || got.Nodes[1].Status != GoalNodePending {
		t.Fatalf("optional node changed unsafely: %#v", got.Nodes[1])
	}
}
