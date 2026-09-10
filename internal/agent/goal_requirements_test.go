package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func requirementTypes(reqs []GoalRequirement) []GoalNodeType {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]GoalNodeType, len(reqs))
	for i, req := range reqs {
		out[i] = req.Type
	}
	return out
}

func TestExtractExplicitRequirements(t *testing.T) {
	tests := []struct {
		name, goal string
		wantTypes  []GoalNodeType
		wantCount  int
	}{
		{"empty", "", nil, 0},
		{"simple implementation", "Fix the parser bug.", []GoalNodeType{GoalNodeImplementation}, 1},
		{"task 11", "Inspect the failing behavior in Average, fix Average(-4, -6) to return -5, run the relevant test, then run go test ./....", []GoalNodeType{GoalNodeExploration, GoalNodeImplementation, GoalNodeVerification, GoalNodeVerification}, 4},
		{"artifact", "Fix the parser bug and create REPORT.md documenting the change.", []GoalNodeType{GoalNodeImplementation, GoalNodeArtifact}, 2},
		{"explore implement verify", "Inspect the parser, implement the fix, and verify it.", []GoalNodeType{GoalNodeExploration, GoalNodeImplementation, GoalNodeVerification}, 3},
		{"single full test", "Run go test ./....", []GoalNodeType{GoalNodeVerification}, 1},
		{"single relevant test", "Run the relevant test.", []GoalNodeType{GoalNodeVerification}, 1},
		{"multiple artifacts", "Create REPORT.md and update TRACE.md.", []GoalNodeType{GoalNodeArtifact, GoalNodeArtifact}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractExplicitRequirements(tt.goal)
			if len(got) != tt.wantCount {
				t.Fatalf("count = %d, want %d: %#v", len(got), tt.wantCount, got)
			}
			if !reflect.DeepEqual(requirementTypes(got), tt.wantTypes) {
				t.Fatalf("types = %v, want %v", requirementTypes(got), tt.wantTypes)
			}
			for _, req := range got {
				if req.ID == "" || req.Description == "" || req.SourceText == "" || !req.Required {
					t.Fatalf("incomplete requirement: %#v", req)
				}
			}
		})
	}
}

func TestExtractRequirementsAreStableAndTraceable(t *testing.T) {
	goal := "Inspect calc, fix Average(-4, -6), then run the relevant test and go test ./...."
	a := ExtractExplicitRequirements(goal)
	b := ExtractExplicitRequirements(goal)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("extraction is not deterministic: %#v != %#v", a, b)
	}
	for _, req := range a {
		if !strings.Contains(goal, req.SourceText) {
			t.Fatalf("source text %q is not traceable to goal", req.SourceText)
		}
	}
}

func TestExtractsTestCreationAlongsideImplementation(t *testing.T) {
	goal := "Fix Average so it handles negative inputs correctly. Add or update focused tests and run go test ./... ."
	reqs := ExtractExplicitRequirements(goal)
	var implementation, testCreation bool
	for _, req := range reqs {
		implementation = implementation || req.Type == GoalNodeImplementation
		testCreation = testCreation || req.Type == GoalNodeTestCreation
	}
	if !implementation || !testCreation {
		t.Fatalf("independent implementation/test requirements were not extracted: %#v", reqs)
	}
}

type fakeGoalGraphProposer struct {
	proposal GoalGraphProposal
	calls    int
}

func (p *fakeGoalGraphProposer) ProposeGoalGraph(_ context.Context, _ string, _ []GoalRequirement) (GoalGraphProposal, error) {
	p.calls++
	return p.proposal, nil
}

func TestGoalGraphProposerBoundaryIsInjectable(t *testing.T) {
	p := &fakeGoalGraphProposer{proposal: GoalGraphProposal{Graph: validSingleGraph()}}
	got, err := p.ProposeGoalGraph(context.Background(), "do work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || got.Graph.Nodes[0].ID != "work" {
		t.Fatalf("proposal boundary not injectable: calls=%d graph=%#v", p.calls, got.Graph)
	}
}
