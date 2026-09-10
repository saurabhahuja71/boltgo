package agent

import (
	"fmt"
	"sort"
	"strings"
)

// GoalGraphStatus describes the structural state of a graph. It is separate
// from AgentPhase; Phase 1 does not connect graphs to the runtime loop.
type GoalGraphStatus string

const (
	GoalGraphActive   GoalGraphStatus = "active"
	GoalGraphComplete GoalGraphStatus = "complete"
	GoalGraphBlocked  GoalGraphStatus = "blocked"
	GoalGraphInvalid  GoalGraphStatus = "invalid"
)

type GoalNodeType string

const (
	GoalNodeExploration    GoalNodeType = "exploration"
	GoalNodeImplementation GoalNodeType = "implementation"
	GoalNodeTestCreation   GoalNodeType = "test_creation"
	GoalNodeTestExecution  GoalNodeType = "test_execution"
	GoalNodeVerification   GoalNodeType = "verification"
	GoalNodeArtifact       GoalNodeType = "artifact"
)

type GoalNodeStatus string

const (
	GoalNodePending   GoalNodeStatus = "pending"
	GoalNodeReady     GoalNodeStatus = "ready"
	GoalNodeRunning   GoalNodeStatus = "running"
	GoalNodeObserved  GoalNodeStatus = "observed"
	GoalNodeSatisfied GoalNodeStatus = "satisfied"
	GoalNodeFailed    GoalNodeStatus = "failed"
	GoalNodeBlocked   GoalNodeStatus = "blocked"
)

// GoalEvidence is a compact reference to externally observable evidence. The
// full tool history remains in AgentRunState; this type is graph coordination
// only and intentionally does not duplicate that history.
type GoalEvidence struct {
	Kind      string
	Source    string
	Summary   string
	Satisfied bool
}

type GoalFailure struct {
	Source    string
	Summary   string
	Retryable bool
}

type GoalNode struct {
	ID          string
	Description string
	Type        GoalNodeType
	Status      GoalNodeStatus
	Required    bool
	// RequirementIDs preserves the deterministic extraction requirements that
	// this proposed node is intended to satisfy. It is coordination metadata;
	// it is not execution evidence.
	RequirementIDs    []string
	DependsOn         []string
	EvidencePredicate string
	Evidence          []GoalEvidence
	Attempts          int
	Failures          []GoalFailure
	Executor          string
}

type GoalGraph struct {
	Version         int
	GoalID          string
	OriginalGoal    string
	Nodes           []GoalNode
	RootIDs         []string
	TerminalIDs     []string
	RequiredNodeIDs []string
	CurrentNodeID   string
	Replans         int
	Status          GoalGraphStatus
}

// ReadinessOptions exposes the future graph-level execution bound without
// coupling Phase 1 to Agent's iteration/tool-call limits.
type ReadinessOptions struct {
	ExecutionAllowed bool
}

func (g GoalGraph) nodeIndex() map[string]int {
	indexes := make(map[string]int, len(g.Nodes))
	for i := range g.Nodes {
		indexes[g.Nodes[i].ID] = i
	}
	return indexes
}

func (g GoalGraph) Validate() error {
	if g.Version < 0 {
		return fmt.Errorf("goal graph version must not be negative")
	}
	indexes := make(map[string]int, len(g.Nodes))
	for i, node := range g.Nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			return fmt.Errorf("node %d has empty ID", i)
		}
		if _, exists := indexes[id]; exists {
			return fmt.Errorf("duplicate node ID %q", id)
		}
		indexes[id] = i
		if !meaningfulDescription(node.Description) {
			return fmt.Errorf("node %q has empty or meaningless description", id)
		}
		if !validNodeType(node.Type) {
			return fmt.Errorf("node %q has invalid type %q", id, node.Type)
		}
		if !validNodeStatus(node.Status) {
			return fmt.Errorf("node %q has invalid status %q", id, node.Status)
		}
		if node.Type == GoalNodeVerification && strings.TrimSpace(node.EvidencePredicate) == "" {
			return fmt.Errorf("verification node %q has no observable evidence predicate", id)
		}
		if node.Attempts < 0 {
			return fmt.Errorf("node %q has negative attempts", id)
		}
	}

	for _, node := range g.Nodes {
		seen := make(map[string]bool, len(node.DependsOn))
		for _, dep := range node.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				return fmt.Errorf("node %q has empty dependency ID", node.ID)
			}
			if dep == node.ID {
				return fmt.Errorf("node %q depends on itself", node.ID)
			}
			if _, ok := indexes[dep]; !ok {
				return fmt.Errorf("node %q has unknown dependency %q", node.ID, dep)
			}
			if seen[dep] {
				return fmt.Errorf("node %q repeats dependency %q", node.ID, dep)
			}
			seen[dep] = true
		}
	}
	if err := g.validateAcyclic(indexes); err != nil {
		return err
	}

	required := g.requiredIDs(indexes)
	for _, id := range g.RequiredNodeIDs {
		id = strings.TrimSpace(id)
		node, ok := indexes[id]
		represented := ok
		if !represented {
			for i := range g.Nodes {
				for _, requirementID := range g.Nodes[i].RequirementIDs {
					if requirementID == id {
						node, represented = i, true
						break
					}
				}
				if represented {
					break
				}
			}
		}
		if !represented {
			return fmt.Errorf("required requirement %q is not represented by a node", id)
		}
		if !g.Nodes[node].Required {
			return fmt.Errorf("required requirement %q maps to an optional node", id)
		}
	}

	roots, err := g.roots(indexes)
	if err != nil {
		return err
	}
	reachable := reachableFrom(roots, g.Nodes, indexes)
	for _, id := range required {
		if !reachable[id] {
			return fmt.Errorf("required node %q is unreachable from graph roots", id)
		}
	}

	terminals, err := g.terminals(indexes)
	if err != nil {
		return err
	}
	if len(required) > 0 && len(terminals) == 0 {
		return fmt.Errorf("required work has no terminal path")
	}
	terminalSet := make(map[string]bool, len(terminals))
	for _, terminal := range terminals {
		terminalSet[terminal] = true
	}
	for _, id := range required {
		if !canReachTerminal(id, terminalSet, g.Nodes, indexes) {
			return fmt.Errorf("required node %q has no terminal path", id)
		}
	}
	for _, terminal := range terminals {
		node := g.Nodes[indexes[terminal]]
		// Optional terminal branches are allowed; they must not block required
		// completion. A required verification terminal, however, must not be
		// able to declare DONE without required implementation work behind it.
		if node.Required && node.Type == GoalNodeVerification && !hasRequiredNonVerificationAncestor(terminal, g.Nodes, indexes) {
			return fmt.Errorf("terminal node %q bypasses required work", terminal)
		}
	}
	for _, node := range g.Nodes {
		if node.Type == GoalNodeVerification && node.Required && !hasRequiredNonVerificationAncestor(node.ID, g.Nodes, indexes) {
			return fmt.Errorf("required verification node %q is disconnected from required work", node.ID)
		}
	}
	return nil
}

func (g GoalGraph) validateAcyclic(indexes map[string]int) error {
	state := make(map[string]uint8, len(indexes))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("dependency cycle includes node %q", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range g.Nodes[indexes[id]].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(indexes))
	for id := range indexes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (g GoalGraph) requiredIDs(indexes map[string]int) []string {
	if len(g.RequiredNodeIDs) > 0 {
		ids := append([]string(nil), g.RequiredNodeIDs...)
		sort.Strings(ids)
		return ids
	}
	ids := []string{}
	for id, i := range indexes {
		if g.Nodes[i].Required {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (g GoalGraph) roots(indexes map[string]int) ([]string, error) {
	if len(g.RootIDs) > 0 {
		roots := append([]string(nil), g.RootIDs...)
		sort.Strings(roots)
		for _, id := range roots {
			i, ok := indexes[id]
			if !ok {
				return nil, fmt.Errorf("unknown root node %q", id)
			}
			if len(g.Nodes[i].DependsOn) != 0 {
				return nil, fmt.Errorf("root node %q has dependencies", id)
			}
		}
		return roots, nil
	}
	roots := []string{}
	for _, node := range g.Nodes {
		if len(node.DependsOn) == 0 {
			roots = append(roots, node.ID)
		}
	}
	sort.Strings(roots)
	return roots, nil
}

func (g GoalGraph) terminals(indexes map[string]int) ([]string, error) {
	outgoing := make(map[string]bool, len(g.Nodes))
	for _, node := range g.Nodes {
		for _, dep := range node.DependsOn {
			outgoing[dep] = true
		}
	}
	if len(g.TerminalIDs) > 0 {
		ids := append([]string(nil), g.TerminalIDs...)
		sort.Strings(ids)
		for _, id := range ids {
			if _, ok := indexes[id]; !ok {
				return nil, fmt.Errorf("unknown terminal node %q", id)
			}
			if outgoing[id] {
				return nil, fmt.Errorf("terminal node %q has dependents", id)
			}
		}
		return ids, nil
	}
	ids := []string{}
	for _, node := range g.Nodes {
		if !outgoing[node.ID] {
			ids = append(ids, node.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func reachableFrom(roots []string, nodes []GoalNode, indexes map[string]int) map[string]bool {
	seen := make(map[string]bool, len(nodes))
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, node := range nodes {
			for _, dep := range node.DependsOn {
				if dep == id {
					walk(node.ID)
				}
			}
		}
	}
	for _, id := range roots {
		if _, ok := indexes[id]; ok {
			walk(id)
		}
	}
	return seen
}

func canReachTerminal(id string, terminals map[string]bool, nodes []GoalNode, indexes map[string]int) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(current string) bool {
		if terminals[current] {
			return true
		}
		if seen[current] {
			return false
		}
		seen[current] = true
		for _, node := range nodes {
			for _, dep := range node.DependsOn {
				if dep == current && walk(node.ID) {
					return true
				}
			}
		}
		return false
	}
	return walk(id)
}

func hasRequiredAncestor(id string, nodes []GoalNode, indexes map[string]int) bool {
	for _, dep := range nodes[indexes[id]].DependsOn {
		if nodes[indexes[dep]].Required || hasRequiredAncestor(dep, nodes, indexes) {
			return true
		}
	}
	return nodes[indexes[id]].Required
}

func hasRequiredNonVerificationAncestor(id string, nodes []GoalNode, indexes map[string]int) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(current string) bool {
		if seen[current] {
			return false
		}
		seen[current] = true
		for _, dep := range nodes[indexes[current]].DependsOn {
			n := nodes[indexes[dep]]
			if n.Required && n.Type != GoalNodeVerification {
				return true
			}
			if walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(id)
}

func (g GoalGraph) NodeReady(id string) (bool, error) {
	return g.NodeReadyWithOptions(id, ReadinessOptions{ExecutionAllowed: true})
}

func (g GoalGraph) NodeReadyWithOptions(id string, options ReadinessOptions) (bool, error) {
	if err := g.Validate(); err != nil {
		return false, err
	}
	if !options.ExecutionAllowed {
		return false, nil
	}
	i, ok := g.nodeIndex()[id]
	if !ok {
		return false, fmt.Errorf("unknown node %q", id)
	}
	node := g.Nodes[i]
	if node.Status == GoalNodeSatisfied || node.Status == GoalNodeBlocked || node.Status == GoalNodeRunning || node.Status == GoalNodeObserved {
		return false, nil
	}
	for _, dep := range node.DependsOn {
		d := g.Nodes[g.nodeIndex()[dep]]
		if d.Status != GoalNodeSatisfied {
			return false, nil
		}
	}
	return node.Status == GoalNodePending || node.Status == GoalNodeReady || node.Status == GoalNodeFailed, nil
}

func (g GoalGraph) ReadyNodeIDs() ([]string, error) {
	return g.ReadyNodeIDsWithOptions(ReadinessOptions{ExecutionAllowed: true})
}

func (g GoalGraph) ReadyNodeIDsWithOptions(options ReadinessOptions) ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, node := range g.Nodes {
		ready, err := g.NodeReadyWithOptionsNoValidate(node.ID, options)
		if err != nil {
			return nil, err
		}
		if ready {
			ids = append(ids, node.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (g GoalGraph) NodeReadyWithOptionsNoValidate(id string, options ReadinessOptions) (bool, error) {
	if !options.ExecutionAllowed {
		return false, nil
	}
	indexes := g.nodeIndex()
	i, ok := indexes[id]
	if !ok {
		return false, fmt.Errorf("unknown node %q", id)
	}
	node := g.Nodes[i]
	if node.Status == GoalNodeSatisfied || node.Status == GoalNodeBlocked || node.Status == GoalNodeRunning || node.Status == GoalNodeObserved {
		return false, nil
	}
	for _, dep := range node.DependsOn {
		if g.Nodes[indexes[dep]].Status != GoalNodeSatisfied {
			return false, nil
		}
	}
	return node.Status == GoalNodePending || node.Status == GoalNodeReady || node.Status == GoalNodeFailed, nil
}

func (g GoalGraph) TransitionNode(id string, to GoalNodeStatus) (GoalGraph, error) {
	indexes := g.nodeIndex()
	i, ok := indexes[id]
	if !ok {
		return g, fmt.Errorf("unknown node %q", id)
	}
	if !validNodeStatus(to) {
		return g, fmt.Errorf("invalid target status %q", to)
	}
	from := g.Nodes[i].Status
	if from == GoalNodeSatisfied {
		return g, fmt.Errorf("satisfied node %q cannot regress without explicit invalidation", id)
	}
	if to == GoalNodeSatisfied && !hasSatisfiedEvidence(g.Nodes[i]) {
		return g, fmt.Errorf("node %q has no satisfied evidence", id)
	}
	if !validTransition(from, to) {
		return g, fmt.Errorf("invalid transition %s -> %s for node %q", from, to, id)
	}
	copy := cloneGoalGraph(g)
	copy.Nodes[i].Status = to
	return copy, nil
}

func hasSatisfiedEvidence(node GoalNode) bool {
	for _, evidence := range node.Evidence {
		if evidence.Satisfied {
			return true
		}
	}
	return false
}

func validTransition(from, to GoalNodeStatus) bool {
	switch from {
	case GoalNodePending:
		return to == GoalNodeReady
	case GoalNodeReady:
		return to == GoalNodeRunning
	case GoalNodeRunning:
		return to == GoalNodeObserved || to == GoalNodeFailed || to == GoalNodeBlocked
	case GoalNodeObserved:
		return to == GoalNodeSatisfied
	case GoalNodeFailed, GoalNodeBlocked:
		return to == GoalNodeReady
	default:
		return false
	}
}

func validNodeStatus(status GoalNodeStatus) bool {
	switch status {
	case GoalNodePending, GoalNodeReady, GoalNodeRunning, GoalNodeObserved, GoalNodeSatisfied, GoalNodeFailed, GoalNodeBlocked:
		return true
	}
	return false
}
func validNodeType(typ GoalNodeType) bool {
	switch typ {
	case GoalNodeExploration, GoalNodeImplementation, GoalNodeTestCreation, GoalNodeTestExecution, GoalNodeVerification, GoalNodeArtifact:
		return true
	}
	return false
}
func meaningfulDescription(description string) bool {
	for _, r := range strings.TrimSpace(description) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

func cloneGoalGraph(g GoalGraph) GoalGraph {
	out := g
	out.Nodes = make([]GoalNode, len(g.Nodes))
	copy(out.Nodes, g.Nodes)
	for i := range out.Nodes {
		out.Nodes[i].DependsOn = append([]string(nil), g.Nodes[i].DependsOn...)
		out.Nodes[i].RequirementIDs = append([]string(nil), g.Nodes[i].RequirementIDs...)
		out.Nodes[i].Evidence = append([]GoalEvidence(nil), g.Nodes[i].Evidence...)
		out.Nodes[i].Failures = append([]GoalFailure(nil), g.Nodes[i].Failures...)
	}
	out.RootIDs = append([]string(nil), g.RootIDs...)
	out.TerminalIDs = append([]string(nil), g.TerminalIDs...)
	out.RequiredNodeIDs = append([]string(nil), g.RequiredNodeIDs...)
	return out
}
