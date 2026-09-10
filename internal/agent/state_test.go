package agent

import (
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func verificationState() AgentRunState {
	var state AgentRunState
	state.reset("fix the implementation and make the regression test pass")
	return state
}

func failedTests() tools.ExecutionResult {
	return tools.ExecutionResult{Output: "FAIL: TestRegression", Category: tools.FailureCommand}
}

func passedTests() tools.ExecutionResult {
	return tools.ExecutionResult{Output: "ok ./...", Category: tools.FailureSuccess}
}

func TestVerificationFailureSurvivesSuccessfulRead(t *testing.T) {
	state := verificationState()
	state.addObservation("run_tests", `{"command":"go test ./..."}`, failedTests())
	state.addObservation("read_file", `{"path":"x.go"}`, tools.ExecutionResult{Output: "contents", Category: tools.FailureSuccess})
	if state.canVerify() || state.VerificationCriteria[0].Status == VerificationPassed {
		t.Fatalf("read cleared failed verification: %+v", state)
	}
}

func TestVerificationFailureSurvivesUnrelatedSuccess(t *testing.T) {
	state := verificationState()
	state.addObservation("run_tests", `{}`, failedTests())
	state.addObservation("find_files", `{"name":"*.go"}`, tools.ExecutionResult{Output: "x.go", Category: tools.FailureSuccess})
	if state.canVerify() {
		t.Fatalf("unrelated success cleared failed verification: %+v", state)
	}
}

func TestFreshSuccessfulRerunVerifiesFailedCriterion(t *testing.T) {
	state := verificationState()
	args := `{"command":"go test ./..."}`
	state.addObservation("run_tests", args, failedTests())
	state.addObservation("read_file", `{"path":"x.go"}`, tools.ExecutionResult{Output: "fixed", Category: tools.FailureSuccess})
	state.addObservation("run_tests", args, passedTests())
	if !state.canVerify() || state.VerificationCriteria[0].Status != VerificationPassed {
		t.Fatalf("fresh successful rerun did not verify: %+v", state)
	}
}

func TestVerificationCriteriaAreIndependent(t *testing.T) {
	state := verificationState()
	state.addObservation("run_tests", `{"command":"go test ./..."}`, passedTests())
	state.addObservation("run_shell", `{"command":"go build ./..."}`, failedTests())
	if state.canVerify() || len(state.VerificationCriteria) != 2 {
		t.Fatalf("one criterion incorrectly verified all criteria: %+v", state)
	}
	state.addObservation("read_file", `{"path":"x.go"}`, tools.ExecutionResult{Output: "fixed", Category: tools.FailureSuccess})
	if state.canVerify() {
		t.Fatalf("read cleared second failed criterion: %+v", state)
	}
	state.addObservation("run_shell", `{"command":"go build ./..."}`, passedTests())
	if !state.canVerify() {
		t.Fatalf("all fresh verification criteria did not pass: %+v", state)
	}
}

func TestBuildFailureSurvivesSuccessfulRead(t *testing.T) {
	state := verificationState()
	state.addObservation("run_shell", `{"command":"go build ./..."}`, failedTests())
	state.addObservation("read_file", `{"path":"main.go"}`, tools.ExecutionResult{Output: "contents", Category: tools.FailureSuccess})
	if state.canVerify() {
		t.Fatalf("read cleared failed build verification: %+v", state)
	}
}

func TestFailedVerificationThenReplanAndFreshSuccess(t *testing.T) {
	state := verificationState()
	state.addObservation("run_tests", `{}`, failedTests())
	state.Plan = "replan after failed tests"
	state.addObservation("str_replace", `{"path":"x.go"}`, tools.ExecutionResult{Output: "updated", Category: tools.FailureSuccess})
	state.addObservation("run_tests", `{}`, passedTests())
	if !state.canVerify() || state.VerificationCriteria[0].Status != VerificationPassed {
		t.Fatalf("replanned fresh verification did not pass: %+v", state)
	}
}

func completionState() AgentRunState {
	return AgentRunState{
		Verification: VerificationPassed,
		AcceptanceCriteriaState: []AcceptanceCriterion{
			{Key: "a", Description: "criterion A", Satisfied: true},
			{Key: "b", Description: "criterion B", Satisfied: true},
			{Key: "c", Description: "criterion C", Satisfied: false},
		},
	}
}

func TestCanCompleteRequiresEveryAcceptanceCriterion(t *testing.T) {
	state := completionState()
	if state.canComplete() {
		t.Fatal("completed verification incorrectly ignored unsatisfied criterion")
	}
	state.AcceptanceCriteriaState[2].Satisfied = true
	if !state.canComplete() {
		t.Fatalf("all acceptance criteria and verification passed but cannot complete: %+v", state)
	}
}

func TestVerificationPassWithMissingAcceptanceCriterionIsIncomplete(t *testing.T) {
	state := completionState()
	if state.Verification != VerificationPassed || state.canComplete() {
		t.Fatalf("verification pass incorrectly completed incomplete goal: %+v", state)
	}
}

func TestAllAcceptanceCriteriaWithBlockedVerificationIsIncomplete(t *testing.T) {
	state := completionState()
	for i := range state.AcceptanceCriteriaState {
		state.AcceptanceCriteriaState[i].Satisfied = true
	}
	state.Verification = VerificationBlocked
	if state.canComplete() {
		t.Fatal("blocked verification incorrectly allowed completion")
	}
}

func TestAllAcceptanceCriteriaAndFreshVerificationComplete(t *testing.T) {
	state := completionState()
	for i := range state.AcceptanceCriteriaState {
		state.AcceptanceCriteriaState[i].Satisfied = true
	}
	state.Verification = VerificationPassed
	if !state.canComplete() {
		t.Fatalf("all criteria and fresh verification should complete: %+v", state)
	}
}

func TestUnrelatedSuccessDoesNotSatisfyMissingAcceptanceCriterion(t *testing.T) {
	state := completionState()
	state.Observations = []Observation{{Tool: "read_file", Summary: "unrelated file", Success: true}}
	if state.canComplete() || state.AcceptanceCriteriaState[2].Satisfied {
		t.Fatal("unrelated success satisfied missing acceptance criterion")
	}
}

func TestAcceptanceCriteriaDetectImplementationAndRegressionTestRequests(t *testing.T) {
	criteria := acceptanceCriteriaForGoal("Fix IsEven and add a regression test, then run go test ./...")
	if len(criteria) != 3 || criteria[0].Key != "implementation" || criteria[1].Key != "tests_added" || criteria[2].Key != "verification" {
		t.Fatalf("criteria=%+v", criteria)
	}
	criteria = acceptanceCriteriaForGoal("Change the greeting and update all affected tests")
	if len(criteria) != 2 || criteria[0].Key != "implementation" || criteria[1].Key != "tests_added" {
		t.Fatalf("criteria=%+v", criteria)
	}
}

func TestAcceptanceCriteriaDetectsTask11Verification(t *testing.T) {
	criteria := acceptanceCriteriaForGoal("First inspect the failing behavior in Average, then fix it so Average(-4, -6) returns -5. Run the relevant test and then go test ./...")
	keys := criterionKeys(criteria)
	if !keys["implementation"] || !keys["verification"] || keys["tests_added"] {
		t.Fatalf("task 11 criteria=%+v", criteria)
	}
}

func TestAcceptanceCriteriaDetectsVerificationVariants(t *testing.T) {
	goals := []string{
		"run the relevant test",
		"run relevant test",
		"run relevant tests",
		"run the tests",
		"run tests",
		"execute the relevant test",
		"execute relevant test",
		"run go test ./...",
		"then go test ./...",
		"and then go test ./...",
	}
	for _, goal := range goals {
		t.Run(goal, func(t *testing.T) {
			if !criterionKeys(acceptanceCriteriaForGoal(goal))["verification"] {
				t.Fatalf("verification criterion missing for %q", goal)
			}
		})
	}
}

func TestAcceptanceCriteriaSeparatesTestCreationExecutionAndImplementation(t *testing.T) {
	tests := []struct {
		goal string
		want map[string]bool
	}{
		{goal: "add a regression test", want: map[string]bool{"tests_added": true}},
		{goal: "run the relevant test", want: map[string]bool{"verification": true}},
		{goal: "implement the fix", want: map[string]bool{"implementation": true}},
		{goal: "implement the fix, run the relevant test and then go test ./...", want: map[string]bool{"implementation": true, "verification": true}},
		{goal: "update tests and run go test ./...", want: map[string]bool{"implementation": true, "tests_added": true, "verification": true}},
	}
	for _, tt := range tests {
		t.Run(tt.goal, func(t *testing.T) {
			got := criterionKeys(acceptanceCriteriaForGoal(tt.goal))
			for key, want := range tt.want {
				if got[key] != want {
					t.Fatalf("criteria=%+v, %s=%t want %t", got, key, got[key], want)
				}
			}
			for _, key := range []string{"implementation", "tests_added", "verification"} {
				if got[key] && !tt.want[key] {
					t.Fatalf("criteria=%+v, unexpected %s criterion", got, key)
				}
			}
		})
	}
}

func criterionKeys(criteria []AcceptanceCriterion) map[string]bool {
	keys := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		keys[criterion.Key] = true
	}
	return keys
}

func TestTestMutationUsesUntruncatedTargetPath(t *testing.T) {
	state := verificationState()
	longContent := strings.Repeat("x", 300)
	state.addObservation("write_file", `{"content":"`+longContent+`","path":"calc/pair_test.go"}`, tools.ExecutionResult{Output: "written", Category: tools.FailureSuccess})
	if !state.AcceptanceCriteriaState[1].Satisfied {
		t.Fatalf("long test-file path was not recognized: %+v", state.ToolCalls[0])
	}
}

func TestTestMutationRecognizesShortTestFilePath(t *testing.T) {
	state := verificationState()
	state.addObservation("write_file", `{"path":"foo_test.go","content":"package foo"}`, tools.ExecutionResult{Output: "written", Category: tools.FailureSuccess})
	if !state.AcceptanceCriteriaState[1].Satisfied {
		t.Fatalf("short test-file path was not recognized: %+v", state.ToolCalls[0])
	}
}

func TestTestMutationRecognizesLongStrReplaceTargetPath(t *testing.T) {
	state := verificationState()
	longReplacement := strings.Repeat("y", 300)
	state.addObservation("str_replace", `{"new_string":"`+longReplacement+`","old_string":"old","path":"calc/calc_test.go"}`, tools.ExecutionResult{Output: "updated", Category: tools.FailureSuccess})
	if !state.AcceptanceCriteriaState[1].Satisfied {
		t.Fatalf("long str_replace test-file path was not recognized: %+v", state.ToolCalls[0])
	}
}

func TestTestMutationRequiresTargetPath(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "non-test source", args: `{"content":"mentions _test.go","path":"calc/calc.go"}`},
		{name: "test substring in content", args: `{"content":"test_helper and test.go","path":"calc/calc.go"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := verificationState()
			state.addObservation("write_file", tt.args, tools.ExecutionResult{Output: "written", Category: tools.FailureSuccess})
			if state.AcceptanceCriteriaState[1].Satisfied {
				t.Fatalf("non-test target produced false positive: %+v", state.ToolCalls[0])
			}
		})
	}
}
