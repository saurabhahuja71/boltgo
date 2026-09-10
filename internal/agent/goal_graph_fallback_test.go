package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fallbackErrorProposer struct {
	err   error
	calls int
}

func (p *fallbackErrorProposer) ProposeGoalGraph(context.Context, string, []GoalRequirement) (GoalGraphProposal, error) {
	p.calls++
	return GoalGraphProposal{}, p.err
}

func TestBuildDeterministicFallbackGraphPreservesTask11Requirements(t *testing.T) {
	goal := "Inspect the failing behavior in Average, fix Average(-4, -6) to return -5, run the relevant test, then run go test ./...."
	requirements := ExtractExplicitRequirements(goal)
	graph, err := BuildDeterministicFallbackGraph(goal, requirements)
	if err != nil {
		t.Fatal(err)
	}
	if graph.OriginalGoal != goal {
		t.Fatalf("goal changed: %q", graph.OriginalGoal)
	}
	if err := graph.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, req := range requirements {
		matches := 0
		for _, node := range graph.Nodes {
			for _, id := range node.RequirementIDs {
				if id == req.ID {
					matches++
					if node.Status != GoalNodePending || len(node.Evidence) != 0 {
						t.Fatalf("fallback node %q was not unsatisfied", node.ID)
					}
				}
			}
		}
		if matches != 1 {
			t.Fatalf("requirement %q mapped %d times", req.ID, matches)
		}
	}
	var verification []GoalNode
	for _, node := range graph.Nodes {
		if node.Type == GoalNodeVerification {
			verification = append(verification, node)
		}
	}
	if len(verification) != 2 || verification[0].RequirementIDs[0] == verification[1].RequirementIDs[0] || verification[0].EvidencePredicate == verification[1].EvidencePredicate {
		t.Fatalf("verification requirements were not independent: %#v", verification)
	}
}

func TestConstructGoalGraphWithFallbackRecoversOnlyProposalFailures(t *testing.T) {
	goal := "Fix the parser and run go test ./...."
	p := &fallbackErrorProposer{err: errors.New("decode goal graph proposal JSON: invalid character")}
	graph, info, err := ConstructGoalGraphWithFallback(context.Background(), goal, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || info.Source != "deterministic-fallback" || info.ProposalRejection == "" {
		t.Fatalf("unexpected activation: calls=%d info=%+v", p.calls, info)
	}
	if graph.OriginalGoal != goal || graph.Status != GoalGraphActive {
		t.Fatalf("fallback did not preserve activation state: %#v", graph)
	}
	if err := graph.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, node := range graph.Nodes {
		if node.Status != GoalNodePending || len(node.Evidence) != 0 {
			t.Fatalf("fallback trusted proposal state: %#v", node)
		}
	}
}

func TestFallbackDiscardsIncompatibleProposal(t *testing.T) {
	goal := "Fix Average and run the relevant test, then run go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	proposal := proposalForRequirements(reqs)
	proposal.Graph.Nodes[0].Type = GoalNodeArtifact
	graph, info, err := ConstructGoalGraphWithFallback(context.Background(), goal, &activationProposer{proposal: proposal})
	if err != nil {
		t.Fatal(err)
	}
	if info.Source != "deterministic-fallback" {
		t.Fatalf("source=%q, want fallback", info.Source)
	}
	if err := graph.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, req := range reqs {
		found := false
		for _, node := range graph.Nodes {
			found = found || containsString(node.RequirementIDs, req.ID)
		}
		if !found {
			t.Fatalf("fallback dropped %q", req.ID)
		}
	}
}

func TestFallbackOrdersPrerequisitesBeforeVerificationMentionedInSameClause(t *testing.T) {
	goal := "Fix the implementation so the relevant test passes, then run go test ./...."
	graph, err := BuildDeterministicFallbackGraph(goal, ExtractExplicitRequirements(goal))
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) < 4 {
		t.Fatalf("nodes=%#v", graph.Nodes)
	}
	for i, want := range []GoalNodeType{GoalNodeExploration, GoalNodeImplementation, GoalNodeVerification, GoalNodeVerification} {
		if graph.Nodes[i].Type != want {
			t.Fatalf("node %d type=%q, want %q; nodes=%#v", i, graph.Nodes[i].Type, want, graph.Nodes)
		}
	}
	if len(graph.Nodes[2].DependsOn) == 0 || len(graph.Nodes[3].DependsOn) == 0 {
		t.Fatalf("verification prerequisites missing: %#v", graph.Nodes)
	}
}

func TestFallbackRejectsUnsupportedGoalAndNonRecoverableErrors(t *testing.T) {
	p := &fallbackErrorProposer{err: errors.New("provider authentication failed")}
	if _, _, err := ConstructGoalGraphWithFallback(context.Background(), "please help", p); err == nil || p.calls != 0 {
		t.Fatalf("unsupported goal was not rejected before proposal: err=%v calls=%d", err, p.calls)
	}
	goal := "Fix the parser and run go test ./...."
	p = &fallbackErrorProposer{err: errors.New("provider authentication failed")}
	if _, _, err := ConstructGoalGraphWithFallback(context.Background(), goal, p); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("provider failure incorrectly recovered: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = &fallbackErrorProposer{err: errors.New("decode goal graph proposal JSON: malformed")}
	if _, _, err := ConstructGoalGraphWithFallback(ctx, goal, p); err == nil || p.calls != 1 {
		t.Fatalf("cancelled activation incorrectly fell back: %v", err)
	}
}

func TestFallbackIsDeterministicAndDoesNotMutateRequirements(t *testing.T) {
	goal := "Create REPORT.md and run go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	original := append([]GoalRequirement(nil), reqs...)
	a, err := BuildDeterministicFallbackGraph(goal, reqs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildDeterministicFallbackGraph(goal, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(reqs, original) {
		t.Fatalf("fallback is nondeterministic or mutated input: equal=%v", reflect.DeepEqual(a, b))
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
