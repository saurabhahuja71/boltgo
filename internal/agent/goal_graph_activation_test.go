package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

type activationProposer struct {
	proposal GoalGraphProposal
	calls    int
}

func (p *activationProposer) ProposeGoalGraph(_ context.Context, _ string, _ []GoalRequirement) (GoalGraphProposal, error) {
	p.calls++
	return p.proposal, nil
}

func TestConstructGoalGraphReconcilesAndResetsUntrustedState(t *testing.T) {
	goal := "  Fix Average and run the relevant test, then run go test ./....  "
	reqs := ExtractExplicitRequirements(goal)
	proposal := proposalForRequirements(reqs)
	proposal.Graph.OriginalGoal = "model supplied goal"
	proposal.Graph.Nodes[0].Status = GoalNodeSatisfied
	proposal.Graph.Nodes[0].Evidence = []GoalEvidence{{Kind: "model", Satisfied: true}}
	p := &activationProposer{proposal: proposal}

	got, err := ConstructGoalGraph(context.Background(), goal, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("proposal calls = %d, want 1", p.calls)
	}
	if got.OriginalGoal != goal {
		t.Fatalf("original goal was rewritten: %q", got.OriginalGoal)
	}
	for _, node := range got.Nodes {
		if node.Status != GoalNodePending {
			t.Fatalf("proposal execution state was trusted: %#v", node)
		}
		for _, evidence := range node.Evidence {
			if evidence.Satisfied {
				t.Fatalf("proposal evidence was trusted: %#v", node)
			}
		}
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConstructGoalGraphKeepsIndependentTask11Verification(t *testing.T) {
	goal := "Inspect Average, fix Average(-4, -6), run the relevant test, then run go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	got, err := ConstructGoalGraph(context.Background(), goal, &activationProposer{proposal: proposalForRequirements(reqs)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequiredNodeIDs) != 4 {
		t.Fatalf("required nodes = %v", got.RequiredNodeIDs)
	}
	var verificationIDs []string
	for _, node := range got.Nodes {
		if node.Type == GoalNodeVerification {
			verificationIDs = append(verificationIDs, node.ID)
		}
	}
	if len(verificationIDs) != 2 || reflect.DeepEqual(verificationIDs[0], verificationIDs[1]) {
		t.Fatalf("verification requirements were not independent: %v", verificationIDs)
	}
}

func TestConstructGoalGraphRejectsIncompleteProposal(t *testing.T) {
	goal := "Fix the parser and run go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	p := proposalForRequirements(reqs)
	p.Graph.Nodes = p.Graph.Nodes[:1]
	if _, err := ConstructGoalGraph(context.Background(), goal, &activationProposer{proposal: p}); err == nil {
		t.Fatal("incomplete proposal was accepted")
	}
}

func TestConstructGoalGraphIsDeterministicAndDoesNotMutateProposal(t *testing.T) {
	goal := "Inspect calc, fix the bug, then run go test ./...."
	reqs := ExtractExplicitRequirements(goal)
	p := proposalForRequirements(reqs)
	original := p.Graph
	a, err := ConstructGoalGraph(context.Background(), goal, &activationProposer{proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ConstructGoalGraph(context.Background(), goal, &activationProposer{proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("identical inputs produced different graphs: %#v != %#v", a, b)
	}
	if !reflect.DeepEqual(p.Graph, original) {
		t.Fatal("construction mutated proposal")
	}
}

func TestConstructGoalGraphRequiresProposerAndGoal(t *testing.T) {
	if _, err := ConstructGoalGraph(context.Background(), " ", &activationProposer{}); err == nil {
		t.Fatal("empty goal was accepted")
	}
	if _, err := ConstructGoalGraph(context.Background(), "fix it", nil); err == nil {
		t.Fatal("nil proposer was accepted")
	}
}

func TestConfiguredGoalGraphProposerUsesExistingChatClient(t *testing.T) {
	reqs := ExtractExplicitRequirements("Fix the parser and run go test ./....")
	proposal := proposalForRequirements(reqs)
	body, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
			t.Fatalf("unexpected LLM request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(mustJSONQuote(body, t)) + `}}]}`))
	}))
	defer server.Close()

	client := llm.New(server.URL, "")
	got, err := (ConfiguredGoalGraphProposer{Client: client, Model: "test-model"}).ProposeGoalGraph(context.Background(), "Fix the parser and run go test ./....", reqs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Graph.Nodes) != len(reqs) {
		t.Fatalf("proposal nodes = %d, want %d", len(got.Graph.Nodes), len(reqs))
	}
}

func mustJSONQuote(value []byte, t *testing.T) []byte {
	t.Helper()
	quoted, err := json.Marshal(string(value))
	if err != nil {
		t.Fatal(err)
	}
	return quoted
}
