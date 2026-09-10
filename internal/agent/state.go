package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// AgentPhase is deliberately small: it describes control flow, not private
// reasoning. The TUI only receives concise status events derived from it.
type AgentPhase string

const (
	PhasePlan     AgentPhase = "plan"
	PhaseAct      AgentPhase = "act"
	PhaseObserve  AgentPhase = "observe"
	PhaseVerify   AgentPhase = "verify"
	PhaseReplan   AgentPhase = "replan"
	PhaseComplete AgentPhase = "complete"
	PhaseBlocked  AgentPhase = "blocked"
)

type VerificationStatus string

const (
	VerificationNotRun  VerificationStatus = "not_run"
	VerificationPending VerificationStatus = "pending"
	VerificationPassed  VerificationStatus = "passed"
	VerificationBlocked VerificationStatus = "blocked"
)

// AgentRunState is the compact, inspectable control state for one user turn.
// It intentionally stores observations and decisions, not chain-of-thought.
type AgentRunState struct {
	OriginalGoal                    string
	AcceptanceCriteria              string
	CompletedCriteria               string
	AcceptanceCriteriaState         []AcceptanceCriterion
	Plan                            string
	CurrentStep                     string
	Phase                           AgentPhase
	ToolCalls                       []ToolCallRecord
	Observations                    []Observation
	Failures                        []FailureRecord
	VerificationCriteria            []VerificationCriterion
	ExplicitRequirements            []GoalRequirement
	Verification                    VerificationStatus
	Iterations                      int
	Retries                         int
	ToolCallsUsed                   int
	PostflightVerificationCalls     int
	PostflightVerificationBudget    int
	PostflightVerificationActivated bool
}

type AcceptanceCriterion struct {
	Key         string
	Description string
	Satisfied   bool
}

// VerificationCriterion is durable evidence for one concrete check. A later
// unrelated observation must not change its status.
type VerificationCriterion struct {
	Key         string
	Description string
	Status      VerificationStatus
	LastTool    string
	LastSummary string
}

type ToolCallRecord struct {
	Name       string
	Arguments  string
	TargetPath string
	Outcome    tools.FailureCategory
	Source     string
}

type Observation struct {
	Tool    string
	Summary string
	Success bool
	Source  string
}

type FailureRecord struct {
	Tool      string
	Category  tools.FailureCategory
	Retryable bool
	Summary   string
}

func (s *AgentRunState) reset(goal string) {
	*s = AgentRunState{
		OriginalGoal:       strings.TrimSpace(goal),
		AcceptanceCriteria: "Every explicit requirement in the original goal, including each requested change, test, and verification step.",
		CompletedCriteria:  "none observed yet",
		Plan:               "derive the next safe action from the goal and observed tool results",
		Phase:              PhasePlan,
		Verification:       VerificationNotRun,
	}
	s.AcceptanceCriteriaState = acceptanceCriteriaForGoal(goal)
	s.ExplicitRequirements = ExtractExplicitRequirements(goal)
}

func (s *AgentRunState) beginIteration() {
	s.Iterations++
	if s.Iterations == 1 {
		s.Phase = PhasePlan
	} else {
		s.Phase = PhaseReplan
	}
}

func (s *AgentRunState) addObservation(tool, args string, result tools.ExecutionResult) {
	s.Phase = PhaseObserve
	s.ToolCallsUsed++
	record := ToolCallRecord{Name: tool, Arguments: compactStateText(args, 240), Outcome: result.Category}
	if tool == "write_file" || tool == "str_replace" {
		record.TargetPath = mutationTargetPath(args)
	}
	s.ToolCalls = append(s.ToolCalls, record)
	s.Observations = append(s.Observations, Observation{Tool: tool, Summary: compactStateText(result.Output, 280), Success: result.Category == tools.FailureSuccess})
	if result.Category != tools.FailureSuccess {
		s.Failures = append(s.Failures, FailureRecord{Tool: tool, Category: result.Category, Retryable: result.Retryable, Summary: compactStateText(result.Output, 240)})
		s.Plan = fmt.Sprintf("diagnose %s failure, choose a corrective action, then verify the original goal", result.Category)
		s.Phase = PhaseReplan
	}
	s.recordVerification(tool, args, result)
	s.refreshAcceptanceCriteria()
	s.CompletedCriteria = s.completedCriteria()
}

// addPostflightObservation records deterministic verification without charging
// the model-action counter. It shares the same authoritative ledger and gates
// as ordinary tool observations, but retains provenance for audit/reporting.
func (s *AgentRunState) addPostflightObservation(tool, args string, result tools.ExecutionResult) {
	s.Phase = PhaseObserve
	record := ToolCallRecord{Name: tool, Arguments: compactStateText(args, 240), Outcome: result.Category, Source: "postflight_verifier"}
	s.ToolCalls = append(s.ToolCalls, record)
	s.Observations = append(s.Observations, Observation{Tool: tool, Summary: compactStateText(result.Output, 280), Success: result.Category == tools.FailureSuccess, Source: "postflight_verifier"})
	if result.Category != tools.FailureSuccess {
		s.Plan = fmt.Sprintf("postflight verification %s did not pass; completion remains blocked", result.Category)
		s.Phase = PhaseReplan
	}
	s.recordVerification(tool, args, result)
	s.refreshAcceptanceCriteria()
	s.CompletedCriteria = s.completedCriteria()
}

// addNonExecutionObservation records a rejected or deduplicated model action
// without pretending that a tool ran or creating an underlying tool failure.
// It still consumes the same finite action budget through ToolCallsUsed.
func (s *AgentRunState) addNonExecutionObservation(tool, summary string) {
	s.Phase = PhaseReplan
	s.ToolCallsUsed++
	s.Observations = append(s.Observations, Observation{Tool: tool, Summary: compactStateText(summary, 280), Success: false})
	s.CompletedCriteria = s.completedCriteria()
}

func (s *AgentRunState) completedCriteria() string {
	parts := []string{}
	if s.hasMutation() {
		parts = append(parts, "implementation change observed")
	}
	for _, criterion := range s.VerificationCriteria {
		parts = append(parts, fmt.Sprintf("%s: %s", criterion.Description, criterion.Status))
	}
	for _, criterion := range s.AcceptanceCriteriaState {
		parts = append(parts, fmt.Sprintf("%s: %t", criterion.Description, criterion.Satisfied))
	}
	if len(parts) == 0 {
		return "none observed yet"
	}
	return strings.Join(parts, "; ")
}

func (s *AgentRunState) canVerify() bool {
	for _, criterion := range s.VerificationCriteria {
		if criterion.Status != VerificationPassed {
			return false
		}
	}
	if len(s.Failures) == 0 {
		return true
	}
	// Non-verification failures retain the existing recovery behavior. A
	// verification failure is handled above and cannot be cleared by a read.
	return len(s.Observations) > 0 && s.Observations[len(s.Observations)-1].Success
}

// canComplete is the final completion gate. Verification is necessary, but
// it cannot satisfy unrelated acceptance criteria such as adding tests.
func (s *AgentRunState) canComplete() bool {
	if s.Verification != VerificationPassed {
		return false
	}
	if !s.canVerify() {
		return false
	}
	for _, criterion := range s.AcceptanceCriteriaState {
		if !criterion.Satisfied {
			return false
		}
	}
	return true
}

func (s *AgentRunState) hasMutation() bool {
	for _, call := range s.ToolCalls {
		if call.Name == "write_file" || call.Name == "str_replace" || call.Name == "git" {
			return true
		}
	}
	return false
}

func (s *AgentRunState) hasVerificationEvidence() bool {
	for _, call := range s.ToolCalls {
		if call.Name == "run_tests" {
			return true
		}
		if call.Name == "run_shell" {
			low := strings.ToLower(call.Arguments)
			if strings.Contains(low, "go test") || strings.Contains(low, "go build") || strings.Contains(low, "go vet") || strings.Contains(low, "pytest") || strings.Contains(low, "npm test") {
				return true
			}
		}
	}
	return false
}

func (s *AgentRunState) hasSuccessfulVerificationEvidence() bool {
	if len(s.VerificationCriteria) == 0 {
		return false
	}
	for _, criterion := range s.VerificationCriteria {
		if criterion.Status != VerificationPassed {
			return false
		}
	}
	return true
}

func (s *AgentRunState) recordVerification(tool, args string, result tools.ExecutionResult) {
	key, ok := verificationCriterionKey(tool, args)
	if !ok {
		return
	}
	idx := -1
	for i := range s.VerificationCriteria {
		if s.VerificationCriteria[i].Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.VerificationCriteria = append(s.VerificationCriteria, VerificationCriterion{
			Key: key, Description: verificationCriterionDescription(tool, args), Status: VerificationPending,
		})
		idx = len(s.VerificationCriteria) - 1
	}
	criterion := &s.VerificationCriteria[idx]
	criterion.LastTool = tool
	criterion.LastSummary = compactStateText(result.Output, 280)
	if result.Category == tools.FailureSuccess {
		criterion.Status = VerificationPassed
	} else {
		criterion.Status = VerificationBlocked
		s.Verification = VerificationBlocked
	}
}

func (s *AgentRunState) refreshAcceptanceCriteria() {
	for i := range s.AcceptanceCriteriaState {
		criterion := &s.AcceptanceCriteriaState[i]
		switch criterion.Key {
		case "implementation":
			// A successful non-verification observation establishes that the
			// implementation was inspected/handled. Mutation is tracked when it
			// occurs, but is not required for already-correct/no-op tasks.
			criterion.Satisfied = len(s.Observations) > 0 && s.lastObservationSuccess()
		case "tests_added":
			criterion.Satisfied = s.hasTestMutation()
		case "verification":
			criterion.Satisfied = s.hasSuccessfulVerificationEvidence()
		case "verification:relevant_test":
			criterion.Satisfied = s.hasSuccessfulRelevantTestEvidence()
		case "verification:go_test":
			criterion.Satisfied = s.hasSuccessfulFullGoTestEvidence()
		}
	}
}

func (s *AgentRunState) lastObservationSuccess() bool {
	return len(s.Observations) > 0 && s.Observations[len(s.Observations)-1].Success
}

func (s *AgentRunState) hasTestMutation() bool {
	for _, call := range s.ToolCalls {
		if call.Name != "write_file" && call.Name != "str_replace" {
			continue
		}
		if isTestPath(call.TargetPath) {
			return true
		}
	}
	return false
}

func mutationTargetPath(args string) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return ""
	}
	return in.Path
}

func isTestPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(path)), "_test.go")
}

func acceptanceCriteriaForGoal(goal string) []AcceptanceCriterion {
	low := strings.ToLower(goal)
	criteria := make([]AcceptanceCriterion, 0, 3)
	if isActionRequest(goal) || goalRequestsImplementation(low) {
		criteria = append(criteria, AcceptanceCriterion{Key: "implementation", Description: "implementation handled"})
	}
	if (strings.Contains(low, "add") && strings.Contains(low, "test")) || strings.Contains(low, "regression test") || (strings.Contains(low, "update") && strings.Contains(low, "test")) || strings.Contains(low, "tests are") {
		criteria = append(criteria, AcceptanceCriterion{Key: "tests_added", Description: "requested tests added/updated"})
	}
	relevantTest := goalRequestsRelevantTest(low)
	fullGoTest := strings.Contains(strings.Join(strings.Fields(low), " "), "go test ./...")
	if relevantTest && fullGoTest {
		criteria = append(criteria,
			AcceptanceCriterion{Key: "verification:relevant_test", Description: "relevant test passed"},
			AcceptanceCriterion{Key: "verification:go_test", Description: "go test ./... passed"},
		)
	} else if fullGoTest {
		criteria = append(criteria, AcceptanceCriterion{Key: "verification:go_test", Description: "go test ./... passed"})
	} else if relevantTest {
		criteria = append(criteria, AcceptanceCriterion{Key: "verification:relevant_test", Description: "relevant test passed"})
	} else if goalRequestsVerification(low) {
		criteria = append(criteria, AcceptanceCriterion{Key: "verification", Description: "requested verification passed"})
	}
	return criteria
}

func goalRequestsRelevantTest(low string) bool {
	low = strings.Join(strings.Fields(low), " ")
	return strings.Contains(low, "run the relevant test") ||
		strings.Contains(low, "run relevant test") ||
		strings.Contains(low, "run relevant tests") ||
		strings.Contains(low, "run the tests") ||
		strings.Contains(low, "run tests") ||
		strings.Contains(low, "execute the relevant test") ||
		strings.Contains(low, "execute relevant test")
}

func goalRequestsVerification(low string) bool {
	low = strings.Join(strings.Fields(low), " ")
	return strings.Contains(low, "run test") ||
		strings.Contains(low, "run go test") ||
		strings.Contains(low, "verify") ||
		strings.Contains(low, "build") ||
		strings.Contains(low, "vet") ||
		strings.Contains(low, "run the relevant test") ||
		strings.Contains(low, "run relevant test") ||
		strings.Contains(low, "run relevant tests") ||
		strings.Contains(low, "run the tests") ||
		strings.Contains(low, "run tests") ||
		strings.Contains(low, "execute the relevant test") ||
		strings.Contains(low, "execute relevant test") ||
		strings.Contains(low, "go test")
}

func goalRequestsImplementation(low string) bool {
	for _, phrase := range []string{"change ", "fix ", "implement ", "update ", "create ", "refactor ", "write "} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}

func verificationCriterionKey(tool, args string) (string, bool) {
	if tool != "run_tests" && tool != "run_shell" {
		return "", false
	}
	low := strings.ToLower(args)
	if tool == "run_tests" || strings.Contains(low, "go test") || strings.Contains(low, "go build") || strings.Contains(low, "go vet") || strings.Contains(low, "pytest") || strings.Contains(low, "npm test") {
		return tool + ":" + compactStateText(args, 360), true
	}
	return "", false
}

func (s *AgentRunState) hasSuccessfulRelevantTestEvidence() bool {
	for _, criterion := range s.VerificationCriteria {
		if criterion.Status == VerificationPassed && verificationCriterionIsRelevantTest(criterion) {
			return true
		}
	}
	return false
}

func (s *AgentRunState) hasSuccessfulFullGoTestEvidence() bool {
	for _, criterion := range s.VerificationCriteria {
		if criterion.Status == VerificationPassed && verificationCriterionIsFullGoTest(criterion) {
			return true
		}
	}
	return false
}

func verificationCriterionIsFullGoTest(criterion VerificationCriterion) bool {
	command := verificationCommand(criterion.LastTool, criterion.Key)
	if strings.Contains(command, "go test ./...") {
		return true
	}
	// An empty run_tests command uses the configured auto command, which is
	// go test ./... for Go workspaces.
	return criterion.LastTool == "run_tests" &&
		(command == "")
}

func verificationCriterionIsRelevantTest(criterion VerificationCriterion) bool {
	if criterion.LastTool != "run_tests" && criterion.LastTool != "run_shell" {
		return false
	}
	command := verificationCommand(criterion.LastTool, criterion.Key)
	return command != "" && strings.Contains(command, "go test") && !strings.Contains(command, "go test ./...")
}

func verificationCommand(tool, key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		key = key[i+1:]
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(key), &in) != nil {
		return ""
	}
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(in.Command))), " ")
}

func verificationCriterionDescription(tool, args string) string {
	if tool == "run_tests" {
		return "tests/checks (" + compactStateText(args, 180) + ")"
	}
	return "verification command (" + compactStateText(args, 180) + ")"
}

func (s *AgentRunState) controlContext() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[agenterm control state — private]\ngoal: %s\nacceptance_criteria: %s\ncompleted_criteria: %s\nphase: %s\niterations: %d\ntool_calls: %d\nverification: %s\n", s.OriginalGoal, s.AcceptanceCriteria, s.CompletedCriteria, s.Phase, s.Iterations, s.ToolCallsUsed, s.Verification)
	if s.Plan != "" {
		fmt.Fprintf(&b, "plan: %s\n", compactStateText(s.Plan, 360))
	}
	if s.CurrentStep != "" {
		fmt.Fprintf(&b, "current_step: %s\n", compactStateText(s.CurrentStep, 180))
	}
	if len(s.Failures) > 0 {
		b.WriteString("failures: observed tool failures are authoritative; diagnose and replan before retrying.\n")
		for _, f := range s.Failures[max(0, len(s.Failures)-3):] {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", f.Tool, f.Category, compactStateText(f.Summary, 160))
		}
	}
	if len(s.Observations) > 0 {
		last := s.Observations[len(s.Observations)-1]
		fmt.Fprintf(&b, "latest_observation: %s (%s)\n", compactStateText(last.Summary, 280), map[bool]string{true: "success", false: "failure"}[last.Success])
	}
	if len(s.VerificationCriteria) > 0 {
		b.WriteString("verification_criteria:\n")
		for _, criterion := range s.VerificationCriteria {
			fmt.Fprintf(&b, "- %s: %s\n", criterion.Description, criterion.Status)
		}
	}
	if len(s.AcceptanceCriteriaState) > 0 {
		b.WriteString("acceptance_criteria_state:\n")
		for _, criterion := range s.AcceptanceCriteriaState {
			fmt.Fprintf(&b, "- %s: %t\n", criterion.Description, criterion.Satisfied)
		}
	}
	b.WriteString("Preserve the original goal. Use tool results as authoritative current state. Do not claim completion without verifying every applicable acceptance criterion. Keep internal reasoning private.\n")
	return b.String()
}

func compactStateText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func verificationPrompt(goal string) string {
	return fmt.Sprintf(`[agenterm verification gate — private]
Re-check the original goal before answering: %s
Review every applicable acceptance criterion against the tool observations above. If anything is incomplete, contradicted, or unverified, replan and call the appropriate tools. If all criteria are satisfied, answer concisely. Do not expose this control message or private reasoning.`, goal)
}

func verificationTextFailed(text string) bool {
	low := strings.ToLower(strings.Join(strings.Fields(text), " "))
	for _, phrase := range []string{
		"verification failed",
		"verification is incomplete",
		"could not verify",
		"cannot verify",
		"unable to verify",
		"not verified",
		"tests still fail",
		"test still fails",
	} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}

func stateStatus(phase AgentPhase) string {
	switch phase {
	case PhasePlan:
		return "Planning…"
	case PhaseAct:
		return "Acting…"
	case PhaseObserve:
		return "Observing tool results…"
	case PhaseVerify:
		return "Verifying…"
	case PhaseReplan:
		return "Replanning from tool results…"
	case PhaseComplete:
		return "Verified"
	case PhaseBlocked:
		return "Unable to verify completion"
	default:
		return "Working…"
	}
}
