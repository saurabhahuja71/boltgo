package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

const compatibilityVerificationReserveSize = 2
const compatibilityPostflightBudget = 2

func (a *Agent) compatibilityVerificationStillMissing() bool {
	return a.CompatibilityMode && len(a.compatibilityMissingVerification()) > 0
}

// compatibilityNonVerificationWorkRemaining prevents the verification
// closure from taking over while implementation, test, artifact, or
// exploration requirements still need ordinary tools.
func (a *Agent) compatibilityNonVerificationWorkRemaining() bool {
	if !a.CompatibilityMode {
		return false
	}
	for _, req := range a.RunState.ExplicitRequirements {
		if req.Type != GoalNodeVerification && !a.compatibilityRequirementSatisfied(req) {
			return true
		}
	}
	for _, criterion := range a.RunState.AcceptanceCriteriaState {
		if criterion.Key == "tests_added" && !criterion.Satisfied {
			return true
		}
	}
	return false
}

func (a *Agent) compatibilityClosureEligible() bool {
	// A failed observation may indicate recoverable implementation work even
	// when the structural requirements have been observed. Let the ordinary
	// loop handle that recovery; closure must never turn a legal repair into a
	// verification-only rejection.
	return a.compatibilityVerificationStillMissing() &&
		!a.compatibilityNonVerificationWorkRemaining() &&
		len(a.RunState.Failures) == 0
}

func (a *Agent) compatibilityReserve(maxToolCalls int) int {
	if maxToolCalls <= 0 || !a.compatibilityVerificationStillMissing() {
		return 0
	}
	reserve := compatibilityVerificationReserveSize
	if maxToolCalls-1 < reserve {
		reserve = maxToolCalls - 1
	}
	return reserve
}

func (a *Agent) beginCompatibilityClosure(emit func(Event)) {
	if a.compatibilityClosureActive || !a.compatibilityClosureEligible() || a.compatibilityVerificationReserve == 0 {
		return
	}
	a.compatibilityClosureActive = true
	emit(Event{Kind: EventStatus, Text: fmt.Sprintf("compatibility verification closure: unresolved requirements remain; reserved actions=%d", a.compatibilityVerificationReserve)})
}

func (a *Agent) finishCompatibilityClosure(emit func(Event)) {
	a.RunState.Phase = PhaseBlocked
	missing := strings.Join(a.compatibilityMissingVerification(), "; ")
	emit(Event{Kind: EventError, Text: "verification closure ended safely without authoritative completion: " + missing})
	emit(Event{Kind: EventDone})
}

func compatibilityVerificationTools(all []llm.Tool) []llm.Tool {
	filtered := make([]llm.Tool, 0, 2)
	for _, tool := range all {
		if tool.Function.Name == "run_tests" || tool.Function.Name == "run_shell" {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func compatibilityVerificationAction(tool, args string) bool {
	return tool == "run_tests" || (tool == "run_shell" && isTestCommand(args))
}

type compatibilityPostflightAction struct{ tool, args string }

// compatibilityPostflightActions derives only deterministic run_tests actions
// from explicit requirements. It has no general shell or model-text fallback.
func (a *Agent) compatibilityPostflightActions() []compatibilityPostflightAction {
	actions := []compatibilityPostflightAction{}
	seen := map[string]bool{}
	for _, req := range a.RunState.ExplicitRequirements {
		if req.Type != GoalNodeVerification || a.compatibilityRequirementSatisfied(req) {
			continue
		}
		text := strings.ToLower(req.Description + " " + req.EvidencePredicate + " " + req.SourceText)
		command := compatibilityExplicitGoTestCommand(text)
		if command == "" && (strings.Contains(text, "relevant") || strings.Contains(text, "full") || strings.Contains(text, "go test ./...")) {
			continue
		}
		args := `{}`
		if command != "" {
			args = `{"command":"` + strings.ReplaceAll(command, `\`, `\\`) + `"}`
		}
		key := "run_tests:" + args
		if !seen[key] {
			seen[key] = true
			actions = append(actions, compatibilityPostflightAction{tool: "run_tests", args: args})
		}
	}
	return actions
}

func compatibilityExplicitGoTestCommand(text string) string {
	idx := strings.Index(text, "go test")
	if idx < 0 {
		return ""
	}
	text = text[idx:]
	for _, delimiter := range []string{"`", "'", `"`, ";", "\n"} {
		if end := strings.Index(text, delimiter); end >= 0 {
			text = text[:end]
		}
	}
	fields := strings.Fields(text)
	if len(fields) < 2 || fields[0] != "go" || fields[1] != "test" {
		return ""
	}
	return strings.Join(fields, " ")
}

func (a *Agent) compatibilityPostflightEligible() bool {
	return a.CompatibilityMode && a.Scheduler == nil && a.Tools != nil && !a.compatibilityMalformedToolMarkup && a.RunState.Phase != PhaseComplete &&
		a.compatibilityVerificationStillMissing() && !a.compatibilityNonVerificationWorkRemaining() &&
		(!a.hasUnresolvedCompatibilityFailure()) && len(a.compatibilityPostflightActions()) > 0
}

func (a *Agent) hasUnresolvedCompatibilityFailure() bool {
	return len(a.RunState.Failures) > 0 && !a.RunState.lastObservationSuccess()
}

// compatibilityTerminalComplete performs the final compatibility projection
// before a bounded terminal failure. It deliberately delegates to the
// existing authoritative compatibility gate; it does not add evidence or
// alter either canVerify or canComplete.
func (a *Agent) compatibilityTerminalComplete(emit func(Event)) bool {
	if !a.CompatibilityMode || a.Scheduler != nil || !a.compatibilityCanComplete() {
		return false
	}
	a.RunState.Phase = PhaseComplete
	emit(Event{Kind: EventStatus, Text: "compatibility mode: authoritative requirements and verification gates satisfied"})
	return true
}

// runCompatibilityPostflight is a terminal read-only verification pass. It is
// not a model turn and does not increment the normal model-action counter.
func (a *Agent) runCompatibilityPostflight(ctx context.Context, emit func(Event)) bool {
	if !a.compatibilityPostflightEligible() || a.RunState.PostflightVerificationActivated {
		return false
	}
	a.RunState.PostflightVerificationActivated = true
	a.RunState.PostflightVerificationBudget = compatibilityPostflightBudget
	emit(Event{Kind: EventStatus, Text: fmt.Sprintf("compatibility postflight verification: unresolved authoritative checks; budget=%d", compatibilityPostflightBudget)})
	actions := a.compatibilityPostflightActions()
	if len(actions) > compatibilityPostflightBudget {
		actions = actions[:compatibilityPostflightBudget]
	}
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			return false
		}
		level, capability := permissions.LevelForTool(action.tool, action.args)
		request := permissions.Request{Tool: action.tool, Capability: capability, Level: level, Arguments: action.args}
		decision, prompt := a.Permissions.Check(request)
		if prompt {
			response := make(chan permissions.Decision, 1)
			emit(Event{Kind: EventPermission, Tool: action.tool, Text: action.args, Permission: &request, Decision: response})
			select {
			case decision = <-response:
			case <-ctx.Done():
				return false
			}
			decision = a.Permissions.Commit(request, decision)
		}
		var result tools.ExecutionResult
		if decision == permissions.Deny {
			result = tools.ExecutionResult{Output: "error: permission denied", Category: tools.FailurePermissionDenied, PermissionDenied: true}
		} else {
			emit(Event{Kind: EventToolStart, Tool: action.tool, Text: action.args})
			result = a.Tools.RunDetailed(ctx, action.tool, action.args)
		}
		a.RunState.PostflightVerificationCalls++
		a.RunState.addPostflightObservation(action.tool, action.args, result)
		emit(Event{Kind: EventToolEnd, Tool: action.tool, ToolOut: result.Output})
		if a.compatibilityCanComplete() {
			a.RunState.Phase = PhaseComplete
			emit(Event{Kind: EventStatus, Text: "compatibility postflight: authoritative verification gates satisfied"})
			return true
		}
	}
	return false
}

// compatibilityOperationalContext is task-oriented guidance for local models.
// It deliberately exposes no graph, node, or model-reasoning protocol.
func (a *Agent) compatibilityOperationalContext() string {
	s := &a.RunState
	var b strings.Builder
	b.WriteString("[compatibility execution guidance]\n")
	b.WriteString("Work directly on the original task with normal legal tools. Follow the ordinary task sequence from the requirements and evidence below.\n")
	fmt.Fprintf(&b, "original_goal: %s\nexplicit_requirements:\n", compactStateText(s.OriginalGoal, 900))
	if len(s.ExplicitRequirements) == 0 {
		b.WriteString("- none extracted; answer ordinary conversation safely\n")
	} else {
		for _, req := range s.ExplicitRequirements {
			state := "pending"
			if a.compatibilityRequirementSatisfied(req) {
				state = "satisfied by authoritative observation"
			}
			fmt.Fprintf(&b, "- %s: %s [%s]\n", req.Description, req.EvidencePredicate, state)
		}
	}
	b.WriteString("missing_verification_actions:\n")
	missing := a.compatibilityMissingVerification()
	if len(missing) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, item := range missing {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	if len(s.Observations) > 0 {
		b.WriteString("authoritative_observations:\n")
		for _, observation := range s.Observations[max(0, len(s.Observations)-3):] {
			fmt.Fprintf(&b, "- %s: %s (%t)\n", observation.Tool, compactStateText(observation.Summary, 180), observation.Success)
		}
	}
	b.WriteString("Use structured normal tool calls. Do not claim a requirement or verification is complete from text alone; continue with a missing legal action when one remains.")
	return capToolResult(b.String(), 5000)
}

func (a *Agent) compatibilityRequirementSatisfied(req GoalRequirement) bool {
	s := &a.RunState
	switch req.Type {
	case GoalNodeVerification:
		return a.compatibilityVerificationSatisfied(req)
	case GoalNodeTestCreation:
		return compatibilityHasSuccessfulTestMutation(s)
	case GoalNodeExploration:
		for _, observation := range s.Observations {
			if observation.Success && isGoalGraphReadTool(observation.Tool) {
				return true
			}
		}
		return false
	case GoalNodeArtifact:
		name := strings.ToLower(artifactName(req))
		for _, call := range s.ToolCalls {
			if call.Outcome == tools.FailureSuccess && strings.Contains(strings.ToLower(call.TargetPath), name) {
				return true
			}
		}
		return false
	case GoalNodeImplementation:
		return compatibilityHasSuccessfulMutation(s)
	default:
		return false
	}
}

func compatibilityHasSuccessfulMutation(s *AgentRunState) bool {
	for _, call := range s.ToolCalls {
		if call.Outcome == tools.FailureSuccess &&
			(call.Name == "write_file" || call.Name == "str_replace" || call.Name == "git") {
			return true
		}
	}
	return false
}

func compatibilityHasSuccessfulTestMutation(s *AgentRunState) bool {
	for _, call := range s.ToolCalls {
		if call.Outcome == tools.FailureSuccess &&
			(call.Name == "write_file" || call.Name == "str_replace") && isTestPath(call.TargetPath) {
			return true
		}
	}
	return false
}

func artifactName(req GoalRequirement) string {
	for _, word := range strings.Fields(req.SourceText) {
		word = strings.Trim(word, "`'\".,:;()[]{}")
		if requirementArtifactPattern.MatchString(word) {
			return word
		}
	}
	return req.ID
}

func (a *Agent) compatibilityVerificationSatisfied(req GoalRequirement) bool {
	text := strings.ToLower(req.Description + " " + req.EvidencePredicate + " " + req.SourceText)
	if strings.Contains(text, "relevant") {
		return a.RunState.hasSuccessfulRelevantTestEvidence()
	}
	if strings.Contains(text, "go test ./...") || strings.Contains(text, "full") {
		return a.RunState.hasSuccessfulFullGoTestEvidence()
	}
	return a.RunState.hasSuccessfulVerificationEvidence()
}

func (a *Agent) compatibilityMissingVerification() []string {
	missing := []string{}
	for _, req := range a.RunState.ExplicitRequirements {
		if req.Type == GoalNodeVerification && !a.compatibilityRequirementSatisfied(req) {
			missing = append(missing, req.Description+" ("+req.EvidencePredicate+")")
		}
	}
	return missing
}

// compatibilityCanComplete adds no new completion rule. It only lets the
// existing gates finish after authoritative evidence, without a ceremonial
// model-only response. canVerify and canComplete remain unchanged.
func (a *Agent) compatibilityCanComplete() bool {
	if !a.CompatibilityMode || len(a.RunState.ExplicitRequirements) == 0 {
		return false
	}
	for _, req := range a.RunState.ExplicitRequirements {
		if !a.compatibilityRequirementSatisfied(req) {
			return false
		}
	}
	if !a.RunState.canVerify() {
		return false
	}
	if a.RunState.Verification != VerificationPassed {
		a.RunState.Verification = VerificationPassed
	}
	return a.RunState.canComplete()
}
