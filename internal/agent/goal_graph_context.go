package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/tools"
)

type goalGraphSnapshot struct {
	NodeID   string
	Status   GoalNodeStatus
	Evidence int
	Replans  int
}

func (a *Agent) goalGraphSnapshot() goalGraphSnapshot {
	if a.Scheduler == nil {
		return goalGraphSnapshot{}
	}
	i, ok := a.Scheduler.Graph.nodeIndex()[a.Scheduler.CurrentNodeID]
	if !ok {
		return goalGraphSnapshot{}
	}
	n := a.Scheduler.Graph.Nodes[i]
	return goalGraphSnapshot{NodeID: n.ID, Status: n.Status, Evidence: len(n.Evidence), Replans: a.Scheduler.Graph.Replans}
}

func (a *Agent) classifyGoalAction(tool string, result tools.ExecutionResult, before goalGraphSnapshot) GoalProgressClass {
	if result.Category != tools.FailureSuccess {
		return GoalProgressExecution
	}
	after := a.goalGraphSnapshot()
	if after.NodeID != before.NodeID || after.Status != before.Status || after.Evidence > before.Evidence || after.Replans > before.Replans {
		return GoalProgressAuthoritative
	}
	if tool == "repo_map" || tool == "list_dir" || tool == "read_file" || tool == "grep" || tool == "find_files" || tool == "write_file" || tool == "str_replace" || tool == "run_tests" || tool == "run_shell" {
		// A successful observation/mutation/test is useful runtime evidence even
		// when this node's predicate does not match it.
		return GoalProgressAuthoritative
	}
	return GoalProgressNone
}

func (a *Agent) recordGoalProgress(class GoalProgressClass, summary, tool string, executed, newProgress bool) {
	if newProgress {
		a.progressRevision++
	}
	a.GoalProgress = append(a.GoalProgress, GoalProgressEvent{Class: class, Tool: tool, Summary: compactStateText(summary, 280), Executed: executed, NewProgress: newProgress, BudgetConsumed: true})
}

func goalActionKey(nodeID, tool, args string) string {
	var value any
	if json.Unmarshal([]byte(args), &value) == nil {
		canonical, _ := json.Marshal(value)
		return nodeID + "\x00" + tool + "\x00" + string(canonical)
	}
	return nodeID + "\x00" + tool + "\x00" + strings.Join(strings.Fields(args), " ")
}

func (a *Agent) isRepeatedGoalAction(actionKey string) bool {
	if actionKey == "" || actionKey != a.lastActionKey || a.progressRevision != a.lastActionRevision {
		return false
	}
	switch a.lastActionClass {
	case GoalProgressAuthoritative, GoalProgressNone, GoalProgressScope, GoalProgressRepeated:
		return true
	default:
		// A failed dispatch remains eligible for the existing bounded retry path.
		return false
	}
}

func (a *Agent) goalGraphOperationalContext(maxIterations, maxToolCalls, maxRetries int) string {
	s := a.Scheduler
	if s == nil {
		return ""
	}
	completed, required := 0, 0
	completedReqs := []string{}
	remaining := []string{}
	blocked := []string{}
	for _, node := range s.Graph.Nodes {
		if !node.Required {
			continue
		}
		required++
		if node.Status == GoalNodeSatisfied {
			completed++
			completedReqs = append(completedReqs, nodeRequirementLabel(node))
		} else if node.Status == GoalNodeBlocked || node.Status == GoalNodeFailed {
			blocked = append(blocked, node.ID+": "+lastNodeFailure(node))
		} else if node.ID != s.CurrentNodeID {
			remaining = append(remaining, nodeRequirementLabel(node))
		}
	}
	sort.Strings(completedReqs)
	sort.Strings(remaining)
	sort.Strings(blocked)
	var b strings.Builder
	fmt.Fprintf(&b, "[goal graph operational context — private]\noriginal_goal: %s\nprogress: %d/%d required nodes satisfied\n", s.Graph.OriginalGoal, completed, required)
	if s.CurrentNodeID != "" {
		i := s.Graph.nodeIndex()[s.CurrentNodeID]
		node := s.Graph.Nodes[i]
		fmt.Fprintf(&b, "current_node: %s\ncurrent_node_satisfied: %t\nCurrent objective: %s\nlegal_capabilities: %s\ntarget_restrictions: %s\n", node.ID, node.Status == GoalNodeSatisfied, compactStateText(node.Description, 360), strings.Join(s.LegalCapabilities(), ", "), s.TargetRestrictions())
	} else {
		fmt.Fprintf(&b, "current_node: none\ncurrent_node_satisfied: false\nCurrent objective: none\nlegal_capabilities: none\ntarget_restrictions: none\n")
	}
	if ready, err := s.Graph.ReadyNodeIDsWithOptions(ReadinessOptions{ExecutionAllowed: s.ExecutionAllowed}); err == nil {
		fmt.Fprintf(&b, "next_ready_node: %s\n", joinOrNone(ready))
	}
	fmt.Fprintf(&b, "completed_requirements: %s\nremaining_requirements: %s\n", joinOrNone(completedReqs), joinOrNone(remaining))
	fmt.Fprintf(&b, "blocked_requirements: %s\n", joinOrNone(blocked))
	for _, node := range s.Graph.Nodes {
		if node.Required && node.Status != GoalNodeSatisfied {
			if reason := dependencyReasonForNode(s.Graph, node.ID); reason != "" {
				fmt.Fprintf(&b, "dependency_block: %s\n", reason)
			}
		}
	}
	if reason := s.DependencyBlockReason(); reason != "" {
		fmt.Fprintf(&b, "dependency_block: %s\n", reason)
	}
	if nodeID := s.CurrentNodeID; nodeID != "" {
		node := s.Graph.Nodes[s.Graph.nodeIndex()[nodeID]]
		if len(node.Evidence) > 0 {
			b.WriteString("evidence_obtained:\n")
			for _, evidence := range node.Evidence[max(0, len(node.Evidence)-3):] {
				fmt.Fprintf(&b, "- %s: %s (%t)\n", evidence.Source, compactStateText(evidence.Summary, 180), evidence.Satisfied)
			}
		}
	}
	if len(a.RunState.Observations) > 0 {
		b.WriteString("recent_evidence:\n")
		for _, observation := range a.RunState.Observations[max(0, len(a.RunState.Observations)-3):] {
			fmt.Fprintf(&b, "- %s: %s (%t)\n", observation.Tool, compactStateText(observation.Summary, 180), observation.Success)
		}
	}
	verification := []string{}
	for _, criterion := range a.RunState.VerificationCriteria {
		if criterion.Status != VerificationPassed {
			verification = append(verification, criterion.Description)
		}
	}
	fmt.Fprintf(&b, "verification_still_required: %s\n", joinOrNone(verification))
	if len(a.GoalProgress) > 0 {
		last := a.GoalProgress[len(a.GoalProgress)-1]
		fmt.Fprintf(&b, "last_action: %s (%s; executed=%t; authoritative_state_changed=%t; new_progress=%t)\n", last.Tool, last.Class, last.Executed, last.NewProgress, last.NewProgress)
		if last.FromNode != "" || last.ToNode != "" {
			fmt.Fprintf(&b, "routing_decision: %s -> %s\n", last.FromNode, last.ToNode)
		}
		if last.Class != GoalProgressAuthoritative {
			fmt.Fprintf(&b, "last_feedback: %s\n", last.Summary)
		}
	} else {
		b.WriteString("last_feedback: none\nprevious_action_authoritative_state_changed: false\n")
	}
	fmt.Fprintf(&b, "remaining_budget: iterations=%d, tool_actions=%d, retries=%d\n", maxInt(0, maxIterations-a.RunState.Iterations), maxInt(0, maxToolCalls-a.RunState.ToolCallsUsed), maxInt(0, maxRetries-a.RunState.Retries))
	b.WriteString("Use only legal capabilities and authoritative evidence. Do not claim completion from an assertion.")
	return capToolResult(b.String(), 6000)
}

func nodeRequirementLabel(node GoalNode) string {
	return node.ID + " (" + compactStateText(node.Description, 180) + ")"
}

func lastNodeFailure(node GoalNode) string {
	if len(node.Failures) == 0 {
		return "unresolved dependency or prior failure"
	}
	return compactStateText(node.Failures[len(node.Failures)-1].Summary, 180)
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func dependencyReasonForNode(graph GoalGraph, nodeID string) string {
	indexes := graph.nodeIndex()
	i, ok := indexes[nodeID]
	if !ok {
		return ""
	}
	for _, dep := range graph.Nodes[i].DependsOn {
		j, exists := indexes[dep]
		if exists && graph.Nodes[j].Status != GoalNodeSatisfied {
			return fmt.Sprintf("%s requires %s (status %s)", nodeID, dep, graph.Nodes[j].Status)
		}
	}
	return ""
}
