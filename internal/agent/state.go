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
	// ProviderTurnInProgress is persisted before a daily request. If it is
	// still set on resume, the prior request was interrupted and is never
	// replayed automatically.
	ProviderTurnInProgress bool
	Interrupted            bool
	ProviderState          string
	ToolInProgress         string
	ToolArguments          string
	UnknownToolOutcome     bool
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
	if result.Category != tools.FailureUnsupported {
		s.ToolCallsUsed++
	}
	record := ToolCallRecord{Name: tool, Arguments: compactStateText(args, 240), Outcome: result.Category}
	if tool == "write_file" || tool == "str_replace" {
		record.TargetPath = mutationTargetPath(args)
	}
	s.ToolCalls = append(s.ToolCalls, record)
	// Diagnosis synthesis needs enough of a log/read result to retain the
	// symbol, its decision code, and the surrounding reason. The old 280-byte
	// cap commonly preserved HOLD while truncating the paired SKIP reason.
	observationLimit := 280
	if diagnosisOriented(s.OriginalGoal) && tool != "" {
		observationLimit = 1200
	}
	observation := compactStateText(result.Output, observationLimit)
	if diagnosisOriented(s.OriginalGoal) {
		observation = compactDiagnosisText(result.Output, observationLimit)
	}
	s.Observations = append(s.Observations, Observation{Tool: tool, Summary: observation, Success: result.Category == tools.FailureSuccess})
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
	if s.requiresVerification() && !s.hasSuccessfulVerificationEvidence() {
		return false
	}
	for _, criterion := range s.VerificationCriteria {
		if criterion.Status != VerificationPassed {
			return false
		}
	}
	if len(s.Failures) == 0 {
		return true
	}
	// A successful mutation on a goal that never asked for verification should
	// not stay blocked forever because of an unrelated unsupported/empty test
	// invocation. Recorded verification criteria above still fail closed.
	if !s.needsToolVerificationEvidence() && s.hasSuccessfulMutation() && len(s.VerificationCriteria) == 0 {
		return true
	}
	// Non-verification failures retain the existing recovery behavior. A
	// verification failure is handled above and cannot be cleared by a read.
	return len(s.Observations) > 0 && s.Observations[len(s.Observations)-1].Success
}

func (s *AgentRunState) requiresVerification() bool {
	for _, criterion := range s.AcceptanceCriteriaState {
		// Independent verification requirements are concrete evidence gates. A
		// generic "verify it" criterion may still be satisfied by the existing
		// no-op confirmation path, but named focused/full test requirements may
		// not pass without corresponding tool evidence.
		if strings.HasPrefix(criterion.Key, "verification:") {
			return true
		}
	}
	return false
}

// needsToolVerificationEvidence is true when completion must wait for successful
// test/build tool evidence. Named gates always require it. A generic "verify it"
// criterion requires it after a mutation, while no-op already-correct paths may
// still confirm in text. Goals with no verification criterion must not loop on
// missing evidence after a successful create/edit.
func (s *AgentRunState) needsToolVerificationEvidence() bool {
	if s.requiresVerification() {
		return true
	}
	hasGenericVerify := false
	for _, criterion := range s.AcceptanceCriteriaState {
		if criterion.Key == "verification" {
			hasGenericVerify = true
			break
		}
	}
	return hasGenericVerify && s.hasMutation()
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
	if !s.explicitExplorationRequirementsComplete() {
		return false
	}
	for _, criterion := range s.AcceptanceCriteriaState {
		if !criterion.Satisfied {
			return false
		}
	}
	return true
}

func (s *AgentRunState) explicitExplorationRequirementsComplete() bool {
	for _, req := range s.ExplicitRequirements {
		if req.Type == GoalNodeExploration && !s.explorationRequirementSatisfied(req) {
			return false
		}
	}
	return true
}

func (s *AgentRunState) explorationRequirementSatisfied(req GoalRequirement) bool {
	terms := requirementEvidenceTerms(req.SourceText)
	for i, call := range s.ToolCalls {
		if call.Outcome != tools.FailureSuccess || !isObservationTool(call.Name) {
			continue
		}
		haystack := strings.ToLower(call.Name + " " + call.Arguments + " " + call.TargetPath)
		if i < len(s.Observations) {
			haystack += " " + strings.ToLower(s.Observations[i].Summary)
		}
		for _, term := range terms {
			if strings.Contains(haystack, term) || strings.Contains(haystack, term+"s") {
				return true
			}
		}
	}
	return false
}

func isObservationTool(name string) bool {
	switch name {
	case "repo_map", "list_dir", "read_file", "find_files", "grep", "fetch", "run_shell", "ssh_execute":
		return true
	default:
		return false
	}
}

func requirementEvidenceTerms(text string) []string {
	stop := map[string]bool{
		"inspect": true, "investigate": true, "collect": true, "determine": true,
		"report": true, "using": true, "existing": true, "actual": true,
		"first": true, "the": true, "and": true, "for": true, "with": true,
	}
	seen := map[string]bool{}
	terms := []string{}
	for _, word := range strings.Fields(strings.ToLower(text)) {
		word = strings.Trim(word, "`'\".,:;()[]{}")
		if len(word) < 3 || stop[word] || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
	}
	return terms
}

func (s *AgentRunState) hasMutation() bool {
	for _, call := range s.ToolCalls {
		if call.Name == "write_file" || call.Name == "str_replace" || call.Name == "git" {
			return true
		}
	}
	return false
}

func (s *AgentRunState) hasSuccessfulMutation() bool {
	for _, call := range s.ToolCalls {
		switch call.Name {
		case "write_file", "str_replace", "git":
			if call.Outcome == "" || call.Outcome == tools.FailureSuccess {
				return true
			}
		}
	}
	return false
}

func (s *AgentRunState) hasSuccessfulGitMutation() bool {
	for _, call := range s.ToolCalls {
		if call.Name == "git" && (call.Outcome == "" || call.Outcome == tools.FailureSuccess) {
			return true
		}
	}
	return false
}

func gitArgsIndicatePush(args string) bool {
	low := strings.ToLower(args)
	return strings.Contains(low, "\"push\"") ||
		strings.Contains(low, " push") ||
		strings.HasPrefix(strings.TrimSpace(low), "push") ||
		strings.Contains(low, "command\":\"push") ||
		strings.Contains(low, "command\": \"push") ||
		strings.Contains(low, "args\":\"push") ||
		strings.Contains(low, "args\": \"push") ||
		strings.Contains(low, "args\":[\"push") ||
		strings.Contains(low, "args\": [\"push")
}

func (s *AgentRunState) hasSuccessfulGitPush() bool {
	for i, call := range s.ToolCalls {
		if call.Name != "git" || !gitArgsIndicatePush(call.Arguments) {
			continue
		}
		if call.Outcome != "" && call.Outcome != tools.FailureSuccess {
			continue
		}
		summary := ""
		if i < len(s.Observations) {
			summary = s.Observations[i].Summary
		}
		low := strings.ToLower(summary)
		if strings.Contains(low, "[exit error") || strings.Contains(low, "! [rejected]") || strings.Contains(low, "rejected") {
			continue
		}
		return true
	}
	return false
}

func (s *AgentRunState) addItemAlreadySatisfied(user string) bool {
	token := extractAddItemToken(user)
	if token == "" {
		return false
	}
	tokenUpper := strings.ToUpper(token)
	for i, call := range s.ToolCalls {
		if call.Name != "read_file" || (call.Outcome != "" && call.Outcome != tools.FailureSuccess) {
			continue
		}
		if !strings.Contains(strings.ToLower(call.Arguments), "underlyings.txt") {
			continue
		}
		if i < len(s.Observations) && strings.Contains(strings.ToUpper(s.Observations[i].Summary), tokenUpper) {
			return true
		}
	}
	return false
}

// simpleMutationReadyToConfirm reports that a create/edit-style goal has landed
// its required on-disk/git work. Callers use this to close tools and demand a
// short confirmation instead of cat/ls loops.
//
// Merely seeing an add-token already listed in underlyings.txt is not enough:
// the user may still need workflow/decision_log diagnosis (for example CE not
// selling) or an explicit git push.
func (s *AgentRunState) simpleMutationReadyToConfirm(user string) bool {
	if s.needsToolVerificationEvidence() {
		return false
	}
	low := strings.ToLower(user)
	wantsPush := strings.Contains(low, "push")
	if goalRequestsGitAction(user) {
		if wantsPush {
			return s.hasSuccessfulGitPush()
		}
		return s.hasSuccessfulGitMutation()
	}
	return s.hasSuccessfulMutation()
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
	// Empty/unconfigured test invocations are not verification evidence. Do not
	// poison simple create/edit goals that never asked for tests.
	if result.Category == tools.FailureUnsupported ||
		strings.Contains(strings.ToLower(result.Output), "no test command configured") {
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
			// A successful mutation or non-verification observation establishes
			// that the implementation was handled. A later unsupported/empty
			// test invocation must not erase earlier successful work.
			criterion.Satisfied = s.hasSuccessfulMutation() || (len(s.Observations) > 0 && s.lastObservationSuccess())
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
	if goalRequestsImplementation(low) || (isActionRequest(goal) && !testOnlyActionRequest(low)) {
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
		strings.Contains(low, "go build") ||
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

// testOnlyActionRequest detects prompts that only ask to add/update tests.
// Those remain action requests for budgeting, but must not invent an
// unrelated implementation acceptance criterion.
func testOnlyActionRequest(low string) bool {
	if goalRequestsImplementation(low) {
		return false
	}
	hasTest := strings.Contains(low, "test")
	hasAddOrUpdate := strings.Contains(low, "add ") || strings.Contains(low, "update ")
	return hasTest && hasAddOrUpdate
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
	if s.Interrupted {
		b.WriteString("provider_turn: interrupted; reconstruct from observations before taking any new action.\n")
	}
	if s.UnknownToolOutcome {
		fmt.Fprintf(&b, "unknown_action: %s %s; do not repeat it; inspect the workspace or wait for explicit user direction.\n", s.ToolInProgress, compactStateText(s.ToolArguments, 180))
	}
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
	if len(s.ExplicitRequirements) > 0 {
		b.WriteString("explicit_requirements:\n")
		for _, req := range s.ExplicitRequirements {
			if req.Type != GoalNodeExploration {
				continue
			}
			state := "pending"
			if s.explorationRequirementSatisfied(req) {
				state = "satisfied by authoritative observation"
			}
			fmt.Fprintf(&b, "- %s [%s]\n", req.Description, state)
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

// compactDiagnosisText keeps record boundaries intact. Flattening CSV/log
// output makes a symbol window accidentally include the preceding symbol's
// decision, which can create a false diagnosis.
func compactDiagnosisText(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) <= n {
		return s
	}
	// Keep the beginning/end for file identity, plus source lines that carry
	// the decision mapping. A large read of decision_audit.py otherwise drops
	// the def/return pair and leaves only an unhelpful filename.
	first := n / 3
	last := n / 4
	markers := []string{"def resolve_", "resolve_exit_action_and_reason", "SKIP_ASSIGNMENT_NOT_REQUIRED", "return \"SKIP\"", "error", "failed", "blocked", "not found", "candidate", "qualified", "workflow", "schedule"}
	selected := make([]string, 0, 12)
	seen := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		low := strings.ToLower(line)
		for _, marker := range markers {
			if strings.Contains(low, strings.ToLower(marker)) {
				line = strings.TrimSpace(line)
				if line != "" && !seen[line] {
					seen[line] = true
					selected = append(selected, line)
				}
				break
			}
		}
	}
	result := s[:first] + "\n…\n"
	for _, line := range selected {
		if len(result)+len(line)+1+last > n {
			break
		}
		result += line + "\n"
	}
	result += "…\n" + s[len(s)-last:]
	return result
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
