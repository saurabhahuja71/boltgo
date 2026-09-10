package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// GoalGraphScheduler is an in-memory, single-agent coordinator. It owns no
// model client and executes no tools; the existing Agent loop remains the
// execution engine for the selected node.
type GoalGraphScheduler struct {
	Graph            GoalGraph
	CurrentNodeID    string
	ExecutionAllowed bool
}

func NewGoalGraphScheduler(graph GoalGraph) (*GoalGraphScheduler, error) {
	if err := graph.Validate(); err != nil {
		return nil, err
	}
	return &GoalGraphScheduler{Graph: cloneGoalGraph(graph), ExecutionAllowed: true}, nil
}

// SelectReady returns one eligible node using stable lexical ID ordering.
func (s *GoalGraphScheduler) SelectReady() (string, bool, error) {
	if err := s.Graph.Validate(); err != nil {
		return "", false, err
	}
	if s.CurrentNodeID != "" {
		i, exists := s.Graph.nodeIndex()[s.CurrentNodeID]
		if exists && s.Graph.Nodes[i].Status == GoalNodeRunning {
			return s.CurrentNodeID, true, nil
		}
	}
	ids, err := s.Graph.ReadyNodeIDsWithOptions(ReadinessOptions{ExecutionAllowed: s.ExecutionAllowed})
	if err != nil {
		return "", false, err
	}
	ready := make([]string, 0, len(ids))
	indexes := s.Graph.nodeIndex()
	for _, id := range ids {
		if s.Graph.Nodes[indexes[id]].Status == GoalNodePending || s.Graph.Nodes[indexes[id]].Status == GoalNodeReady {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		return "", false, nil
	}
	return ready[0], true, nil
}

// StartNext transitions exactly one selected node to RUNNING.
func (s *GoalGraphScheduler) StartNext() (string, bool, error) {
	id, ok, err := s.SelectReady()
	if err != nil || !ok {
		return id, ok, err
	}
	if s.CurrentNodeID == id && s.Graph.Nodes[s.Graph.nodeIndex()[id]].Status == GoalNodeRunning {
		return id, true, nil
	}
	if s.Graph.Nodes[s.Graph.nodeIndex()[id]].Status == GoalNodePending {
		if s.Graph, err = s.Graph.TransitionNode(id, GoalNodeReady); err != nil {
			return "", false, err
		}
	}
	if s.Graph, err = s.Graph.TransitionNode(id, GoalNodeRunning); err != nil {
		return "", false, err
	}
	s.CurrentNodeID = id
	s.Graph.CurrentNodeID = id
	s.Graph.Nodes[s.Graph.nodeIndex()[id]].Attempts++
	return id, true, nil
}

// ObserveCurrent records authoritative evidence and advances the current node
// to OBSERVED. Only evidence explicitly marked satisfied can advance it to
// SATISFIED; model assertions never enter this method.
func (s *GoalGraphScheduler) ObserveCurrent(evidence GoalEvidence) error {
	if s.CurrentNodeID == "" {
		return fmt.Errorf("no current goal graph node")
	}
	if strings.EqualFold(strings.TrimSpace(evidence.Kind), "model") || strings.EqualFold(strings.TrimSpace(evidence.Kind), "assistant") {
		return fmt.Errorf("model assertions are not execution evidence")
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok {
		return fmt.Errorf("unknown current node %q", s.CurrentNodeID)
	}
	if s.Graph.Nodes[i].Status != GoalNodeRunning {
		return fmt.Errorf("current node %q is not running", s.CurrentNodeID)
	}
	updated, err := s.Graph.TransitionNode(s.CurrentNodeID, GoalNodeObserved)
	if err != nil {
		return err
	}
	updated.Nodes[updated.nodeIndex()[s.CurrentNodeID]].Evidence = append(updated.Nodes[updated.nodeIndex()[s.CurrentNodeID]].Evidence, evidence)
	s.Graph = updated
	if evidence.Satisfied {
		if s.Graph, err = s.Graph.TransitionNode(s.CurrentNodeID, GoalNodeSatisfied); err != nil {
			return err
		}
		s.CurrentNodeID = ""
		s.Graph.CurrentNodeID = ""
		if s.RequiredComplete() {
			s.Graph.Status = GoalGraphComplete
		}
	}
	return nil
}

func (s *GoalGraphScheduler) FailCurrent(failure GoalFailure) error {
	return s.finishFailure(GoalNodeFailed, failure)
}
func (s *GoalGraphScheduler) BlockCurrent(failure GoalFailure) error {
	return s.finishFailure(GoalNodeBlocked, failure)
}

func (s *GoalGraphScheduler) finishFailure(status GoalNodeStatus, failure GoalFailure) error {
	if s.CurrentNodeID == "" {
		return fmt.Errorf("no current goal graph node")
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok {
		return fmt.Errorf("unknown current node %q", s.CurrentNodeID)
	}
	if s.Graph.Nodes[i].Status != GoalNodeRunning {
		return fmt.Errorf("current node %q is not running", s.CurrentNodeID)
	}
	g, err := s.Graph.TransitionNode(s.CurrentNodeID, status)
	if err != nil {
		return err
	}
	idx := g.nodeIndex()[s.CurrentNodeID]
	g.Nodes[idx].Failures = append(g.Nodes[idx].Failures, failure)
	g.Status = GoalGraphBlocked
	s.Graph = g
	return nil
}

func (s *GoalGraphScheduler) RetryCurrent() error {
	if s.CurrentNodeID == "" {
		return fmt.Errorf("no current goal graph node")
	}
	if s.Graph.Nodes[s.Graph.nodeIndex()[s.CurrentNodeID]].Status != GoalNodeFailed && s.Graph.Nodes[s.Graph.nodeIndex()[s.CurrentNodeID]].Status != GoalNodeBlocked {
		return fmt.Errorf("current node %q is not retryable", s.CurrentNodeID)
	}
	g, err := s.Graph.TransitionNode(s.CurrentNodeID, GoalNodeReady)
	if err != nil {
		return err
	}
	g.Status = GoalGraphActive
	s.Graph = g
	return nil
}

func (s *GoalGraphScheduler) RequiredComplete() bool {
	for _, node := range s.Graph.Nodes {
		if node.Required && node.Status != GoalNodeSatisfied {
			return false
		}
	}
	return true
}

func (s *GoalGraphScheduler) BlockedRequired() bool {
	if s.RequiredComplete() {
		return false
	}
	_, ready, err := s.SelectReady()
	active := false
	if i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]; ok {
		active = s.Graph.Nodes[i].Status == GoalNodeRunning
	}
	return err == nil && !ready && !active
}

func (s *GoalGraphScheduler) CurrentObjective() string {
	if s.CurrentNodeID == "" {
		return ""
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok {
		return ""
	}
	completed, required := 0, 0
	for _, node := range s.Graph.Nodes {
		if node.Required {
			required++
			if node.Status == GoalNodeSatisfied {
				completed++
			}
		}
	}
	remaining := []string{}
	for _, node := range s.Graph.Nodes {
		if node.Required && node.Status != GoalNodeSatisfied && node.ID != s.CurrentNodeID {
			remaining = append(remaining, node.ID)
		}
	}
	sort.Strings(remaining)
	return fmt.Sprintf("Current Goal Graph progress: completed %d/%d required nodes; current node: %s; remaining: %s\nCurrent objective: %q", completed, required, s.CurrentNodeID, strings.Join(remaining, ", "), s.Graph.Nodes[i].Description)
}

// AllowsTool reports whether a tool is in the execution scope of the current
// node. Read-only inspection is available to implementation-like nodes so the
// existing agent can gather the context it needs, but work for later nodes is
// never executed early.
func (s *GoalGraphScheduler) AllowsTool(tool, args string) bool {
	if s == nil || s.CurrentNodeID == "" {
		return false
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok || s.Graph.Nodes[i].Status != GoalNodeRunning {
		return false
	}
	if isGoalGraphReadTool(tool) {
		return true
	}
	return s.allowsToolForNode(s.Graph.Nodes[i], tool, args)
}

// RouteAction selects exactly one ready node for an otherwise scope-rejected
// action. It is deliberately conservative: only pending/ready nodes with all
// dependencies satisfied participate, and a target must match the node type.
func (s *GoalGraphScheduler) RouteAction(tool, args string) (string, bool, error) {
	if s == nil || s.CurrentNodeID == "" {
		return "", false, nil
	}
	if s.AllowsTool(tool, args) {
		return s.CurrentNodeID, false, nil
	}
	ready, err := s.Graph.ReadyNodeIDsWithOptions(ReadinessOptions{ExecutionAllowed: s.ExecutionAllowed})
	if err != nil {
		return "", false, err
	}
	candidates := []string{}
	indexes := s.Graph.nodeIndex()
	for _, id := range ready {
		if id == s.CurrentNodeID {
			continue
		}
		node := s.Graph.Nodes[indexes[id]]
		if (node.Status == GoalNodePending || node.Status == GoalNodeReady) && s.allowsToolForNode(node, tool, args) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) != 1 {
		return "", false, nil
	}
	id := candidates[0]
	if s.Graph.Nodes[indexes[id]].Status == GoalNodePending {
		if s.Graph, err = s.Graph.TransitionNode(id, GoalNodeReady); err != nil {
			return "", false, err
		}
	}
	if s.Graph, err = s.Graph.TransitionNode(id, GoalNodeRunning); err != nil {
		return "", false, err
	}
	s.CurrentNodeID = id
	s.Graph.CurrentNodeID = id
	s.Graph.Nodes[s.Graph.nodeIndex()[id]].Attempts++
	return id, true, nil
}

func (s *GoalGraphScheduler) allowsToolForNode(node GoalNode, tool, args string) bool {
	if isGoalGraphReadTool(tool) {
		return node.Type == GoalNodeExploration || node.Type == GoalNodeImplementation || node.Type == GoalNodeTestCreation || node.Type == GoalNodeArtifact
	}
	switch node.Type {
	case GoalNodeImplementation:
		// Test files belong to test_creation, never implementation routing.
		return (tool == "write_file" || tool == "str_replace") && mutationTargetPath(args) != "" && !isTestPath(mutationTargetPath(args)) || tool == "git"
	case GoalNodeTestCreation:
		return (tool == "write_file" || tool == "str_replace") && isTestPath(mutationTargetPath(args))
	case GoalNodeArtifact:
		return (tool == "write_file" || tool == "str_replace") && mutationTargetPath(args) != ""
	case GoalNodeTestExecution, GoalNodeVerification:
		return tool == "run_tests" || (tool == "run_shell" && isTestCommand(args))
	default:
		return false
	}
}

// TargetRestrictions is compact guidance, not an authorization decision.
func (s *GoalGraphScheduler) TargetRestrictions() string {
	if s == nil || s.CurrentNodeID == "" {
		return "none"
	}
	node := s.Graph.Nodes[s.Graph.nodeIndex()[s.CurrentNodeID]]
	switch node.Type {
	case GoalNodeImplementation:
		return "write_file/str_replace: implementation paths only, never *_test.go; git: repository action"
	case GoalNodeTestCreation:
		return "write_file/str_replace: *_test.go only"
	case GoalNodeArtifact:
		return "write_file/str_replace: explicit non-empty target path"
	case GoalNodeTestExecution, GoalNodeVerification:
		return "run_tests or run_shell with a recognized test command"
	default:
		return "read-only repository inspection"
	}
}

// LegalCapabilities returns stable, model-facing capability categories for
// the current node. It is guidance only; AllowsTool remains authoritative.
func (s *GoalGraphScheduler) LegalCapabilities() []string {
	if s == nil || s.CurrentNodeID == "" {
		return nil
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok {
		return nil
	}
	caps := []string{"inspect"}
	switch s.Graph.Nodes[i].Type {
	case GoalNodeImplementation:
		caps = append(caps, "modify_implementation")
	case GoalNodeTestCreation:
		caps = append(caps, "modify_test")
	case GoalNodeArtifact:
		caps = append(caps, "create_artifact")
	case GoalNodeTestExecution, GoalNodeVerification:
		caps = append(caps, "execute_tests")
	}
	return caps
}

// ScopeFeedback explains a rejected action without exposing graph internals.
func (s *GoalGraphScheduler) ScopeFeedback(tool, _ string) string {
	if s == nil {
		return "goal_graph_scope_violation: no active graph node; no tool was executed"
	}
	reason := "the current node does not permit this capability"
	if s.CurrentNodeID == "" {
		reason = "no node is currently selected"
	} else if i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]; ok {
		node := s.Graph.Nodes[i]
		if len(node.DependsOn) > 0 {
			reason = fmt.Sprintf("current node %q is active; dependencies are already enforced; legal capabilities: %s", node.ID, strings.Join(s.LegalCapabilities(), ", "))
		} else {
			reason = fmt.Sprintf("current node %q permits only: %s", node.ID, strings.Join(s.LegalCapabilities(), ", "))
		}
	}
	return fmt.Sprintf("goal_graph_scope_violation: %s is outside current node %q; %s; no tool was executed", tool, s.CurrentNodeID, reason)
}

// DependencyBlockReason returns the first stable prerequisite explanation for
// a pending current node, if one exists.
func (s *GoalGraphScheduler) DependencyBlockReason() string {
	if s == nil || s.CurrentNodeID == "" {
		return ""
	}
	i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
	if !ok {
		return ""
	}
	for _, dep := range s.Graph.Nodes[i].DependsOn {
		d, exists := s.Graph.nodeIndex()[dep]
		if exists && s.Graph.Nodes[d].Status != GoalNodeSatisfied {
			return fmt.Sprintf("%s requires %s (status %s)", s.CurrentNodeID, dep, s.Graph.Nodes[d].Status)
		}
	}
	return ""
}

func isGoalGraphReadTool(tool string) bool {
	switch tool {
	case "repo_map", "list_dir", "read_file", "grep", "find_files":
		return true
	default:
		return false
	}
}

func isGoalGraphMutationTool(tool string) bool {
	return tool == "write_file" || tool == "str_replace" || tool == "git"
}

func isTestCommand(args string) bool {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return false
	}
	command := strings.ToLower(strings.Join(strings.Fields(in.Command), " "))
	return strings.Contains(command, "go test") || strings.Contains(command, "pytest") || strings.Contains(command, "npm test")
}

// StatusSummary is a compact user-facing view of graph progress. It is
// observational only; the scheduler remains the source of graph state.
func (s *GoalGraphScheduler) StatusSummary() string {
	completed, required := 0, 0
	for _, node := range s.Graph.Nodes {
		if !node.Required {
			continue
		}
		required++
		if node.Status == GoalNodeSatisfied {
			completed++
		}
	}
	remaining := required - completed
	state := string(s.Graph.Status)
	if s.BlockedRequired() {
		state = fmt.Sprintf("blocked (%s)", s.blockReason())
	}
	current := s.CurrentNodeID
	if current == "" {
		current = "none"
	}
	return fmt.Sprintf("goal graph: state=%s; current=%s; completed=%d/%d required; remaining=%d", state, current, completed, required, remaining)
}

func (s *GoalGraphScheduler) blockReason() string {
	if s.CurrentNodeID != "" {
		return "current node unavailable"
	}
	return "required dependency or execution path unavailable"
}

func (a *Agent) startGoalGraphNode() (string, bool, error) {
	if a.Scheduler == nil {
		return "", false, nil
	}
	s := a.Scheduler
	if s.CurrentNodeID != "" {
		i, ok := s.Graph.nodeIndex()[s.CurrentNodeID]
		if ok && (s.Graph.Nodes[i].Status == GoalNodeFailed || s.Graph.Nodes[i].Status == GoalNodeBlocked) {
			if err := s.RetryCurrent(); err != nil {
				return "", false, err
			}
		}
	}
	return s.StartNext()
}

// projectGoalGraphEvidence projects only successful, authoritative runtime
// observations. A model's final text never calls this method.
func (a *Agent) projectGoalGraphEvidence(tool, args string, result tools.ExecutionResult) {
	if a.Scheduler == nil || a.Scheduler.CurrentNodeID == "" {
		return
	}
	idx, ok := a.Scheduler.Graph.nodeIndex()[a.Scheduler.CurrentNodeID]
	if !ok {
		return
	}
	node := a.Scheduler.Graph.Nodes[idx]
	if result.Category != tools.FailureSuccess {
		_ = a.Scheduler.FailCurrent(GoalFailure{Source: tool, Summary: compactStateText(result.Output, 240), Retryable: result.Retryable})
		return
	}
	if !a.goalGraphEvidenceMatches(node, tool, args) {
		return
	}
	_ = a.Scheduler.ObserveCurrent(GoalEvidence{Kind: "runtime", Source: tool, Summary: compactStateText(result.Output, 280), Satisfied: true})
}

func (a *Agent) goalGraphEvidenceMatches(node GoalNode, tool, args string) bool {
	switch node.Type {
	case GoalNodeExploration:
		return explorationEvidenceMatches(node, tool)
	case GoalNodeImplementation:
		return tool == "write_file" || tool == "str_replace" || tool == "git"
	case GoalNodeTestCreation:
		return (tool == "write_file" || tool == "str_replace") && isTestPath(mutationTargetPath(args))
	case GoalNodeArtifact:
		return (tool == "write_file" || tool == "str_replace") && mutationTargetPath(args) != ""
	case GoalNodeTestExecution, GoalNodeVerification:
		predicate := strings.ToLower(node.EvidencePredicate)
		if strings.Contains(predicate, "relevant") {
			return a.RunState.hasSuccessfulRelevantTestEvidence()
		}
		if strings.Contains(predicate, "full") || strings.Contains(predicate, "go test ./...") {
			return a.RunState.hasSuccessfulFullGoTestEvidence()
		}
		return a.RunState.hasSuccessfulVerificationEvidence()
	}
	return false
}

// Discovery tools provide different kinds of evidence. Match them against
// the node's declared predicate/objective instead of allowing any discovery
// result to satisfy every exploration task.
func explorationEvidenceMatches(node GoalNode, tool string) bool {
	// Graphs created before predicate-aware discovery evidence used an empty
	// predicate. Preserve their existing read/discovery behavior while making
	// explicitly scoped predicates selective.
	if strings.TrimSpace(node.EvidencePredicate) == "" {
		return isGoalGraphReadTool(tool)
	}
	text := strings.ToLower(node.Description + " " + node.EvidencePredicate)
	switch tool {
	case "repo_map":
		return containsAny(text, "repo", "repository", "project", "structure", "tree", "discover", "explor")
	case "list_dir":
		return containsAny(text, "directory", "filesystem", "file system", "workspace", "path", "files", "repo", "repository", "discover", "explor")
	case "read_file":
		return containsAny(text, "source", "content", "file", "implementation", "inspect", "read", "code")
	case "grep", "find_files":
		return containsAny(text, "search", "find", "locate", "code", "source", "inspect", "discover", "explor")
	default:
		return false
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
