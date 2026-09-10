package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

// ConstructGoalGraph is the explicit Phase 4 activation boundary. The
// proposer is untrusted: extraction, reconciliation, and Phase 1 validation
// all complete before a graph can be handed to the runtime scheduler.
func ConstructGoalGraph(ctx context.Context, goal string, proposer GoalGraphProposer) (GoalGraph, error) {
	if strings.TrimSpace(goal) == "" {
		return GoalGraph{}, fmt.Errorf("goal graph requires a non-empty goal")
	}
	if proposer == nil {
		return GoalGraph{}, fmt.Errorf("goal graph proposer is not configured")
	}
	requirements := ExtractExplicitRequirements(goal)
	proposal, err := proposer.ProposeGoalGraph(ctx, goal, requirements)
	if err != nil {
		return GoalGraph{}, fmt.Errorf("obtain goal graph proposal: %w", err)
	}
	graph, err := ReconcileGoalGraph(goal, requirements, proposal)
	if err != nil {
		return GoalGraph{}, fmt.Errorf("reconcile goal graph proposal: %w", err)
	}
	if err := graph.Validate(); err != nil {
		return GoalGraph{}, fmt.Errorf("validate goal graph: %w", err)
	}
	return graph, nil
}

// ConfiguredGoalGraphProposer adapts the existing configured chat client to
// the Phase 2 proposal boundary. It does not execute tools or graph nodes.
type ConfiguredGoalGraphProposer struct {
	Client      *llm.Client
	Model       string
	Temperature float64
	MaxTokens   int
}

// GoalGraphActivationInfo records how an adaptive activation was obtained.
// It is diagnostic metadata only and is not part of AgentRunState or graph
// execution state.
type GoalGraphActivationInfo struct {
	Source                  string
	ProposalRejection       string
	RequirementIDs          []string
	FallbackValidationError string
}

// ConstructGoalGraphWithFallback is the v2 activation boundary. The strict
// ConstructGoalGraph contract remains unchanged; this entry point is used by
// normal opt-in execution to recover only from malformed or structurally
// unusable model proposals.
func ConstructGoalGraphWithFallback(ctx context.Context, goal string, proposer GoalGraphProposer) (GoalGraph, GoalGraphActivationInfo, error) {
	if strings.TrimSpace(goal) == "" {
		return GoalGraph{}, GoalGraphActivationInfo{}, fmt.Errorf("goal graph requires a non-empty goal")
	}
	if proposer == nil {
		return GoalGraph{}, GoalGraphActivationInfo{}, fmt.Errorf("goal graph proposer is not configured")
	}
	requirements := ExtractExplicitRequirements(goal)
	info := GoalGraphActivationInfo{Source: "model-proposal", RequirementIDs: requirementIDs(requirements)}
	if len(requirements) == 0 {
		return GoalGraph{}, info, fmt.Errorf("deterministic requirement extraction found no supported executable requirement")
	}
	proposal, proposalErr := proposer.ProposeGoalGraph(ctx, goal, requirements)
	reconcileFailure := false
	if proposalErr == nil {
		graph, reconcileErr := ReconcileGoalGraph(goal, requirements, proposal)
		if reconcileErr == nil {
			return graph, info, nil
		}
		// A decoded proposal that cannot be reconciled is, by definition, an
		// unusable proposal. The fallback is allowed to replace it wholesale;
		// no model graph data is carried into the fallback.
		proposalErr = fmt.Errorf("reconcile goal graph proposal: %w", reconcileErr)
		info.ProposalRejection = proposalErr.Error()
		reconcileFailure = true
	} else if !recoverableProposalFailure(ctx, proposalErr) {
		return GoalGraph{}, info, proposalErr
	}
	if proposalErr == nil || (!reconcileFailure && !recoverableProposalFailure(ctx, proposalErr)) {
		return GoalGraph{}, info, proposalErr
	}
	info.Source = "deterministic-fallback"
	if info.ProposalRejection == "" {
		info.ProposalRejection = proposalErr.Error()
	}
	graph, fallbackErr := BuildDeterministicFallbackGraph(goal, requirements)
	if fallbackErr != nil {
		info.FallbackValidationError = fallbackErr.Error()
		return GoalGraph{}, info, fmt.Errorf("goal graph fallback failed after proposal rejection (%s): %w", info.ProposalRejection, fallbackErr)
	}
	return graph, info, nil
}

func requirementIDs(requirements []GoalRequirement) []string {
	ids := make([]string, len(requirements))
	for i := range requirements {
		ids[i] = requirements[i].ID
	}
	return ids
}

func recoverableProposalFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, marker := range []string{"authentication", "api key", "unauthorized", "forbidden", "connection refused", "deadline exceeded", "timeout", "context canceled", "context cancelled", "rate limit", "http 5"} {
		if strings.Contains(low, marker) {
			return false
		}
	}
	return strings.Contains(low, "decode") || strings.Contains(low, "reconcile") || strings.Contains(low, "validate") || strings.Contains(low, "proposal") || strings.Contains(low, "unknown requirement") || strings.Contains(low, "unsupported")
}

// BuildDeterministicFallbackGraph creates one required node per extracted
// requirement. Dependencies are derived only from requirement categories and
// source order; no model node, edge, status, or evidence is copied.
func BuildDeterministicFallbackGraph(goal string, requirements []GoalRequirement) (GoalGraph, error) {
	if strings.TrimSpace(goal) == "" {
		return GoalGraph{}, fmt.Errorf("fallback requires a non-empty goal")
	}
	if err := validateRequirements(requirements); err != nil {
		return GoalGraph{}, fmt.Errorf("fallback requirements are unsupported: %w", err)
	}
	if len(requirements) == 0 {
		return GoalGraph{}, fmt.Errorf("fallback has no supported explicit requirements")
	}

	work := append([]GoalRequirement(nil), requirements...)
	// Extraction intentionally preserves source order, but a clause can
	// explicitly mention an action and the test it enables (for example,
	// "fix X so the relevant test passes"). Normalize only the deterministic
	// prerequisite order here; ties retain source order.
	sort.SliceStable(work, func(i, j int) bool {
		return fallbackTypeRank(work[i].Type) < fallbackTypeRank(work[j].Type)
	})
	hasExploration := false
	for _, req := range work {
		hasExploration = hasExploration || req.Type == GoalNodeExploration
	}
	if !hasExploration && fallbackNeedsExploration(work) {
		work = append([]GoalRequirement{{
			ID: "fallback-explore", Description: "inspect the repository for task context", Type: GoalNodeExploration,
			Required: true, EvidencePredicate: "repository discovery evidence", SourceText: goal, Ordinal: -1,
		}}, work...)
	}

	nodes := make([]GoalNode, len(work))
	for i, req := range work {
		nodes[i] = GoalNode{
			ID: "fallback-" + req.ID, Description: req.Description, Type: req.Type,
			Status: GoalNodePending, Required: req.Required, RequirementIDs: requirementIDs([]GoalRequirement{req}),
			EvidencePredicate: req.EvidencePredicate,
		}
	}
	for i := range nodes {
		nodes[i].DependsOn = fallbackDependencies(i, nodes)
	}

	graph := GoalGraph{Version: 1, GoalID: "fallback-goal", OriginalGoal: goal, Nodes: nodes, Status: GoalGraphActive}
	for _, node := range nodes {
		if len(node.DependsOn) == 0 {
			graph.RootIDs = append(graph.RootIDs, node.ID)
		}
		if node.Required {
			graph.RequiredNodeIDs = append(graph.RequiredNodeIDs, node.ID)
		}
	}
	for _, node := range nodes {
		isPrerequisite := false
		for _, other := range nodes {
			for _, dep := range other.DependsOn {
				if dep == node.ID {
					isPrerequisite = true
				}
			}
		}
		if !isPrerequisite {
			graph.TerminalIDs = append(graph.TerminalIDs, node.ID)
		}
	}
	if err := graph.Validate(); err != nil {
		return GoalGraph{}, fmt.Errorf("validate deterministic fallback graph: %w", err)
	}
	return graph, nil
}

func fallbackNeedsExploration(requirements []GoalRequirement) bool {
	for _, req := range requirements {
		if req.Type != GoalNodeExploration {
			return true
		}
	}
	return false
}

func fallbackTypeRank(typ GoalNodeType) int {
	switch typ {
	case GoalNodeExploration:
		return 0
	case GoalNodeImplementation:
		return 1
	case GoalNodeTestCreation:
		return 2
	case GoalNodeArtifact:
		return 3
	case GoalNodeTestExecution:
		return 4
	case GoalNodeVerification:
		return 5
	default:
		return 6
	}
}

func fallbackDependencies(index int, nodes []GoalNode) []string {
	node := nodes[index]
	deps := []string{}
	for j := 0; j < index; j++ {
		previous := nodes[j]
		switch node.Type {
		case GoalNodeImplementation:
			if previous.Type == GoalNodeExploration {
				deps = append(deps, previous.ID)
			}
		case GoalNodeTestCreation:
			if previous.Type == GoalNodeExploration || previous.Type == GoalNodeImplementation {
				deps = append(deps, previous.ID)
			}
		case GoalNodeTestExecution, GoalNodeVerification:
			if previous.Type == GoalNodeExploration || previous.Type == GoalNodeImplementation || previous.Type == GoalNodeTestCreation || previous.Type == GoalNodeTestExecution || previous.Type == GoalNodeVerification {
				deps = append(deps, previous.ID)
			}
		case GoalNodeArtifact:
			if previous.Type == GoalNodeExploration || previous.Type == GoalNodeImplementation {
				deps = append(deps, previous.ID)
			}
		}
	}
	return uniqueStrings(deps)
}

func (p ConfiguredGoalGraphProposer) ProposeGoalGraph(ctx context.Context, goal string, requirements []GoalRequirement) (GoalGraphProposal, error) {
	if p.Client == nil {
		return GoalGraphProposal{}, fmt.Errorf("LLM client is not configured")
	}
	requirementsJSON, err := json.Marshal(requirements)
	if err != nil {
		return GoalGraphProposal{}, fmt.Errorf("encode explicit requirements: %w", err)
	}
	request := llm.ChatRequest{
		Model: p.Model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: goalGraphProposalSystemPrompt},
			{Role: llm.RoleUser, Content: fmt.Sprintf("Original goal:\n%s\n\nExplicit requirements (must all be represented):\n%s", goal, requirementsJSON)},
		},
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
	}
	message, err := p.Client.Chat(ctx, request)
	if err != nil {
		return GoalGraphProposal{}, fmt.Errorf("request goal graph proposal: %w", err)
	}
	proposal, err := decodeGoalGraphProposal(message.Content)
	if err != nil {
		return GoalGraphProposal{}, err
	}
	return proposal, nil
}

const goalGraphProposalSystemPrompt = `You propose a candidate Goal Graph for a coding task. Output JSON only, with no markdown and no explanation.
Use this shape: {"graph":{"version":1,"goalID":"goal","nodes":[{"id":"stable-id","description":"...","type":"exploration|implementation|test_creation|test_execution|verification|artifact","status":"pending","required":true,"requirementIDs":["..."],"dependsOn":["..."],"evidencePredicate":"..."}],"rootIDs":[...],"terminalIDs":[...]}}
Treat the original goal and explicit requirements as authoritative. Include only defensible work. All nodes must start pending and verification nodes must have observable evidence predicates. Do not claim execution evidence or completion.`

func decodeGoalGraphProposal(content string) (GoalGraphProposal, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) < 3 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "```") || strings.TrimSpace(lines[len(lines)-1]) != "```" {
			return GoalGraphProposal{}, fmt.Errorf("decode goal graph proposal: malformed JSON fence")
		}
		content = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	var proposal GoalGraphProposal
	if err := json.Unmarshal([]byte(content), &proposal); err == nil && len(proposal.Graph.Nodes) > 0 {
		return proposal, nil
	}
	var graph GoalGraph
	if err := json.Unmarshal([]byte(content), &graph); err != nil {
		return GoalGraphProposal{}, fmt.Errorf("decode goal graph proposal JSON: %w", err)
	}
	return GoalGraphProposal{Graph: graph}, nil
}
