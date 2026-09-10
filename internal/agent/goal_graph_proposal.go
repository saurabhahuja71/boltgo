package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// GoalGraphProposal is intentionally just a transport boundary. Its graph is
// untrusted and must be reconciled before it can be considered a candidate.
type GoalGraphProposal struct {
	Graph GoalGraph
}

// GoalGraphProposer is injectable so Phase 2 can be tested without a model or
// provider. Implementations must only propose; they do not execute nodes.
type GoalGraphProposer interface {
	ProposeGoalGraph(context.Context, string, []GoalRequirement) (GoalGraphProposal, error)
}

// ReconcileGoalGraph maps explicit requirements to an untrusted proposal and
// applies the Phase 1 validator. It is pure: neither input is modified.
func ReconcileGoalGraph(goal string, requirements []GoalRequirement, proposal GoalGraphProposal) (GoalGraph, error) {
	reqs := append([]GoalRequirement(nil), requirements...)
	if strings.TrimSpace(goal) == "" && len(reqs) != 0 {
		return GoalGraph{}, fmt.Errorf("empty goal cannot have requirements")
	}
	if err := validateRequirements(reqs); err != nil {
		return GoalGraph{}, err
	}
	g := cloneGoalGraph(proposal.Graph)
	// Preserve the caller's exact goal for auditability. Validation uses
	// trimmed text where appropriate, but graph traceability must not rewrite
	// the user's input.
	g.OriginalGoal = goal
	g.Status = GoalGraphActive
	if len(g.Nodes) == 0 && len(reqs) > 0 {
		return GoalGraph{}, fmt.Errorf("proposal dropped all explicit requirements")
	}
	reqByID := make(map[string]GoalRequirement, len(reqs))
	for _, req := range reqs {
		reqByID[req.ID] = req
	}
	matched := make(map[string]bool, len(reqs))
	for i := range g.Nodes {
		node := &g.Nodes[i]
		if !validNodeStatus(node.Status) {
			return GoalGraph{}, fmt.Errorf("node %q has invalid proposed status %q", node.ID, node.Status)
		}
		if node.Status != GoalNodePending {
			node.Status = GoalNodePending
		}
		for j := range node.Evidence {
			node.Evidence[j].Satisfied = false
		}
		ids, err := mapNodeRequirements(*node, reqs, reqByID)
		if err != nil {
			return GoalGraph{}, err
		}
		node.RequirementIDs = ids
		for _, id := range ids {
			matched[id] = true
		}
		if node.Required && len(ids) == 0 && !defensiblePrerequisite(*node, goal, reqs) {
			return GoalGraph{}, fmt.Errorf("required proposed node %q is unsupported by the goal", node.ID)
		}
		for _, id := range ids {
			if reqByID[id].Required && !node.Required {
				return GoalGraph{}, fmt.Errorf("required requirement %q is mapped only to optional node %q", id, node.ID)
			}
		}
	}
	for _, req := range reqs {
		if req.Required && !matched[req.ID] {
			return GoalGraph{}, fmt.Errorf("explicit requirement %q is not mapped by proposal", req.ID)
		}
	}
	g.RequiredNodeIDs = requiredGraphNodeIDs(g.Nodes)
	if err := g.Validate(); err != nil {
		return GoalGraph{}, fmt.Errorf("reconciled graph validation: %w", err)
	}
	return g, nil
}

func validateRequirements(reqs []GoalRequirement) error {
	seen := map[string]bool{}
	for _, req := range reqs {
		if strings.TrimSpace(req.ID) == "" || !meaningfulDescription(req.Description) || !meaningfulDescription(req.SourceText) {
			return fmt.Errorf("requirement has incomplete traceability")
		}
		if seen[req.ID] {
			return fmt.Errorf("duplicate requirement ID %q", req.ID)
		}
		seen[req.ID] = true
		if !validNodeType(req.Type) {
			return fmt.Errorf("requirement %q has invalid type %q", req.ID, req.Type)
		}
	}
	return nil
}

func requiredGraphNodeIDs(nodes []GoalNode) []string {
	ids := []string{}
	for _, node := range nodes {
		if node.Required {
			ids = append(ids, node.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func mapNodeRequirements(node GoalNode, reqs []GoalRequirement, byID map[string]GoalRequirement) ([]string, error) {
	if len(node.RequirementIDs) > 0 {
		ids := append([]string(nil), node.RequirementIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			req, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("node %q references unknown requirement %q", node.ID, id)
			}
			if !compatibleRequirementType(req.Type, node.Type) {
				return nil, fmt.Errorf("node %q has incompatible type for requirement %q", node.ID, id)
			}
		}
		return uniqueStrings(ids), nil
	}
	// Without an explicit binding, require a unique deterministic match. This
	// permits wording differences while refusing ambiguous semantic guesses.
	best := []string{}
	bestScore := 0
	for _, req := range reqs {
		if !compatibleRequirementType(req.Type, node.Type) {
			continue
		}
		score := requirementMatchScore(req, node)
		if score > bestScore {
			bestScore = score
			best = []string{req.ID}
		} else if score > 0 && score == bestScore {
			best = append(best, req.ID)
		}
	}
	if bestScore == 0 {
		return nil, nil
	}
	if len(best) != 1 {
		return nil, fmt.Errorf("node %q ambiguously maps requirements %v", node.ID, best)
	}
	return best, nil
}

func compatibleRequirementType(reqType, nodeType GoalNodeType) bool {
	if reqType == nodeType {
		return true
	}
	return (reqType == GoalNodeVerification || reqType == GoalNodeTestExecution) && (nodeType == GoalNodeVerification || nodeType == GoalNodeTestExecution)
}

func requirementMatchScore(req GoalRequirement, node GoalNode) int {
	reqWords := significantWords(req.Description + " " + req.SourceText)
	nodeWords := significantWords(node.Description)
	if len(reqWords) == 0 || len(nodeWords) == 0 {
		return 0
	}
	score := 0
	for word := range reqWords {
		if nodeWords[word] {
			score++
		}
	}
	return score
}

func defensiblePrerequisite(node GoalNode, goal string, reqs []GoalRequirement) bool {
	if node.Type != GoalNodeExploration {
		return false
	}
	goalWords := significantWords(goal)
	nodeWords := significantWords(node.Description)
	for word := range nodeWords {
		if goalWords[word] {
			return true
		}
		for _, req := range reqs {
			if significantWords(req.Description + " " + req.SourceText)[word] {
				return true
			}
		}
	}
	return false
}

func significantWords(s string) map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(strings.ToLower(s)) {
		word = strings.Trim(word, "`'\".,:;()[]{}")
		if len(word) >= 3 && word != "the" && word != "and" && word != "then" && word != "with" {
			words[word] = true
		}
	}
	return words
}

func uniqueStrings(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
