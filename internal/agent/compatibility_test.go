package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestCompatibilityModeUsesLegacyExecutionWithoutGraphProtocol(t *testing.T) {
	a := &Agent{}
	a.EnableCompatibilityMode()
	a.RunState.reset("fix calc.go, add tests, and run the relevant test and go test ./...")
	if a.Scheduler != nil {
		t.Fatal("compatibility mode created a Goal Graph scheduler")
	}
	if len(a.RunState.ExplicitRequirements) < 4 {
		t.Fatalf("requirements were dropped: %#v", a.RunState.ExplicitRequirements)
	}
	ctx := a.compatibilityOperationalContext()
	if strings.Contains(strings.ToLower(ctx), "dag") || strings.Contains(strings.ToLower(ctx), "node protocol") {
		t.Fatalf("compatibility context exposed graph protocol: %s", ctx)
	}
	for _, req := range ExtractExplicitRequirements(a.RunState.OriginalGoal) {
		if !strings.Contains(ctx, req.Description) {
			t.Fatalf("context dropped requirement %q: %s", req.Description, ctx)
		}
	}
}

func TestCompatibilityClaimsCannotSatisfyRequirements(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("implement calc.go, add tests, and run go test ./...")
	a.RunState.Verification = VerificationPassed
	if a.compatibilityCanComplete() {
		t.Fatal("model/state claims completed compatibility task without observations")
	}
	if a.RunState.Verification != VerificationPassed {
		t.Fatal("test setup unexpectedly changed verification")
	}
}

func TestCompatibilityTerminalProjectionCompletesTask14Shape(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("Refactor Add to use a private add helper, add a test, and run go test ./...")
	a.RunState.addObservation("str_replace", `{"path":"calc/calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
	a.RunState.addObservation("str_replace", `{"path":"calc/calc_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
	a.RunState.addObservation("run_tests", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if a.RunState.Verification != VerificationNotRun {
		t.Fatalf("test setup unexpectedly projected verification: %s", a.RunState.Verification)
	}
	if !a.compatibilityTerminalComplete(func(Event) {}) || a.RunState.Phase != PhaseComplete || a.RunState.Verification != VerificationPassed {
		t.Fatalf("terminal projection did not complete authoritative task: %+v", a.RunState)
	}
}

func TestCompatibilityTerminalProjectionCompletesTask17Shape(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("implement NormalizePair in a new file, add unit tests for both orderings, and run go test ./... successfully")
	a.RunState.addObservation("write_file", `{"path":"calc/normalize.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("write_file", `{"path":"calc/normalize_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("run_tests", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if !a.compatibilityTerminalComplete(func(Event) {}) || a.RunState.Phase != PhaseComplete {
		t.Fatalf("task17-shaped evidence did not project completion: %+v", a.RunState)
	}
}

func TestCompatibilityTerminalProjectionPreservesIncompleteAndFailedWork(t *testing.T) {
	cases := []struct {
		name  string
		goal  string
		setup func(*Agent)
	}{
		{name: "missing test creation", goal: "fix calc.go, add tests, and run go test ./...", setup: func(a *Agent) {
			a.RunState.addObservation("str_replace", `{"path":"calc/calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
			a.RunState.addObservation("run_tests", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
		}},
		{name: "unresolved unrelated failure", goal: "add Version, add version_test.go, update README.md, and run go test ./...", setup: func(a *Agent) {
			a.RunState.addObservation("str_replace", `{"path":"calc/calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
			a.RunState.addObservation("write_file", `{"path":"calc/version_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
			a.RunState.addObservation("str_replace", `{"path":"README.md"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
			a.RunState.addObservation("run_tests", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
			a.RunState.addObservation("complete_todo", `{}`, tools.ExecutionResult{Category: tools.FailureUnknown, Output: "todo not found"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{CompatibilityMode: true}
			a.RunState.reset(tc.goal)
			tc.setup(a)
			if a.compatibilityTerminalComplete(func(Event) {}) || a.RunState.Phase == PhaseComplete {
				t.Fatalf("unsafe terminal projection completed: %+v", a.RunState)
			}
		})
	}
}

func TestCompatibilityTerminalProjectionDoesNotStartPostflightWithAuthoritativeVerification(t *testing.T) {
	checker := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(checker)
	a := &Agent{CompatibilityMode: true, Tools: reg, Permissions: permissions.New(permissions.ModeAllow, t.TempDir()+"/permissions.json")}
	a.RunState.reset("refactor Add, add a test, and run go test ./...")
	a.RunState.addObservation("str_replace", `{"path":"calc/calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
	a.RunState.addObservation("str_replace", `{"path":"calc/calc_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "updated"})
	a.RunState.addObservation("run_tests", `{}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if a.runCompatibilityPostflight(context.Background(), func(Event) {}) || len(checker.calls) != 0 {
		t.Fatalf("postflight ran despite authoritative verification: calls=%d state=%+v", len(checker.calls), a.RunState)
	}
}

func TestCompatibilityRelevantAndFullVerificationStayIndependent(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go, run the relevant test, and go test ./...")
	a.RunState.recordVerification("run_tests", `{"command":"go test ./calc -v"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	a.RunState.refreshAcceptanceCriteria()
	if !a.RunState.hasSuccessfulRelevantTestEvidence() || a.RunState.hasSuccessfulFullGoTestEvidence() {
		t.Fatalf("relevant/full evidence was not independent: %#v", a.RunState.VerificationCriteria)
	}
	if a.compatibilityCanComplete() {
		t.Fatal("relevant test alone completed full verification")
	}
	a.RunState.recordVerification("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if !a.RunState.hasSuccessfulRelevantTestEvidence() || !a.RunState.hasSuccessfulFullGoTestEvidence() {
		t.Fatalf("both verification predicates were not recorded: %#v", a.RunState.VerificationCriteria)
	}
}

func TestCompatibilityAuthoritativeEvidenceReachesUnchangedGates(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go, add tests, and run go test ./...")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("write_file", `{"path":"calc_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./calc -v"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if !a.compatibilityCanComplete() || !a.RunState.canVerify() || !a.RunState.canComplete() {
		t.Fatalf("authoritative evidence did not reach existing gates: %+v", a.RunState)
	}
}

func TestCompatibilityIncompleteWorkRemainsIncomplete(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go, add tests, and run go test ./...")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if a.compatibilityCanComplete() {
		t.Fatal("incomplete test requirement or relevant verification completed the task")
	}
}

func TestCompatibilityModeDoesNotChangeToolSafetyBoundary(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	if a.Scheduler != nil || a.CompatibilityMode == false {
		t.Fatal("compatibility mode activation state is invalid")
	}
	// Tool authorization remains owned by the normal registry/permission path;
	// compatibility mode adds no alternate dispatcher or scope allowlist.
	if a.compatibilityCanComplete() {
		t.Fatal("empty compatibility state completed")
	}
}

func TestCompatibilityPostflightCompletesFromAuthoritativeVerification(t *testing.T) {
	checker := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(checker)
	a := &Agent{CompatibilityMode: true, Tools: reg, Permissions: permissions.New(permissions.ModeAllow, t.TempDir()+"/permissions.json")}
	a.RunState.reset("fix calc.go and run the relevant test: go test ./calc -v")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	if !a.runCompatibilityPostflight(context.Background(), func(Event) {}) {
		t.Fatalf("postflight did not reach unchanged gates: %+v", a.RunState)
	}
	if len(checker.calls) != 1 || a.RunState.ToolCallsUsed != 1 || a.RunState.PostflightVerificationCalls != 1 || a.RunState.Phase != PhaseComplete {
		t.Fatalf("unexpected postflight state: calls=%d state=%+v", len(checker.calls), a.RunState)
	}
	if len(a.RunState.Observations) != 2 || a.RunState.Observations[1].Source != "postflight_verifier" || a.RunState.ToolCalls[1].Source != "postflight_verifier" {
		t.Fatalf("postflight provenance missing: %+v", a.RunState)
	}
	if a.RunState.Observations[1].Tool != "run_tests" {
		t.Fatalf("postflight used unexpected tool: %+v", a.RunState.Observations[1])
	}
}

func TestCompatibilityPostflightFailureAndBudgetRemainBounded(t *testing.T) {
	checker := &scriptedTool{name: "run_tests", outputs: []scriptedOutcome{{out: "failed", err: context.DeadlineExceeded}}}
	reg := tools.NewRegistry()
	reg.Register(checker)
	a := &Agent{CompatibilityMode: true, Tools: reg, Permissions: permissions.New(permissions.ModeAllow, t.TempDir()+"/permissions.json")}
	a.RunState.reset("fix calc.go and run the relevant test: go test ./calc -v")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	if a.runCompatibilityPostflight(context.Background(), func(Event) {}) {
		t.Fatal("failed postflight completed")
	}
	if a.RunState.PostflightVerificationCalls != 1 || a.RunState.PostflightVerificationBudget != compatibilityPostflightBudget || len(checker.calls) != 1 || a.compatibilityCanComplete() {
		t.Fatalf("postflight failure/budget not fail-closed: %+v", a.RunState)
	}
}

func TestCompatibilityPostflightDoesNotBypassApproval(t *testing.T) {
	checker := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(checker)
	a := &Agent{CompatibilityMode: true, Tools: reg, Permissions: permissions.New(permissions.ModeAsk, t.TempDir()+"/permissions.json")}
	a.RunState.reset("fix calc.go and run the relevant test: go test ./calc -v")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.runCompatibilityPostflight(context.Background(), func(event Event) {
		if event.Kind == EventPermission {
			event.Decision <- permissions.Deny
		}
	})
	if len(checker.calls) != 0 || a.RunState.PostflightVerificationCalls != 1 || a.compatibilityCanComplete() {
		t.Fatalf("postflight bypassed approval or completed: calls=%d state=%+v", len(checker.calls), a.RunState)
	}
}

func TestCompatibilityKeepsClosureReserveLatent(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("run go test ./...")
	a.compatibilityVerificationReserve = a.compatibilityReserve(10)
	if a.compatibilityVerificationReserve != 2 {
		t.Fatalf("reserve=%d, want 2", a.compatibilityVerificationReserve)
	}
	a.beginCompatibilityClosure(func(Event) {})
	if !a.compatibilityClosureActive || a.compatibilityClosureTurnUsed {
		t.Fatalf("closure state=%+v", a)
	}
	if len(compatibilityVerificationTools([]llm.Tool{{Function: llm.ToolFunctionSchema{Name: "write_file"}}, {Function: llm.ToolFunctionSchema{Name: "run_tests"}}})) != 1 {
		t.Fatal("closure exposed unrelated implementation tool")
	}
}

func TestCompatibilityClosureWaitsForNonVerificationRequirements(t *testing.T) {
	cases := []string{
		"implement calc.go and run go test ./...",
		"add tests for calc.go and run go test ./...",
		"write REPORT.md and run go test ./...",
		"inspect the repository and run go test ./...",
	}
	for _, goal := range cases {
		a := &Agent{CompatibilityMode: true}
		a.RunState.reset(goal)
		a.compatibilityVerificationReserve = a.compatibilityReserve(10)
		a.beginCompatibilityClosure(func(Event) {})
		if a.compatibilityClosureActive {
			t.Fatalf("closure activated with unfinished work for %q", goal)
		}
	}
}

func TestCompatibilityClosureEligibleAfterNonVerificationWork(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go, add tests, and run go test ./...")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("write_file", `{"path":"calc_test.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.compatibilityVerificationReserve = a.compatibilityReserve(10)
	if !a.compatibilityClosureEligible() {
		t.Fatal("closure was not eligible after non-verification work completed")
	}
	a.beginCompatibilityClosure(func(Event) {})
	if !a.compatibilityClosureActive {
		t.Fatal("closure did not activate for unresolved verification")
	}
}

func TestCompatibilityClosureWaitsForFailureRecovery(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go and run go test ./...")
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureCommand, Output: "compile failure"})
	a.compatibilityVerificationReserve = a.compatibilityReserve(10)
	if a.compatibilityClosureEligible() {
		t.Fatal("closure activated while failure recovery remained possible")
	}
}

func TestCompatibilityRejectedMutationCannotSatisfyWork(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("add tests and run go test ./...")
	a.RunState.addObservation("str_replace", `{"path":"calc/calc_test.go"}`, tools.ExecutionResult{Category: tools.FailureUnsupported, Output: "rejected"})
	a.compatibilityVerificationReserve = a.compatibilityReserve(10)
	if !a.compatibilityNonVerificationWorkRemaining() || a.compatibilityClosureEligible() {
		t.Fatal("rejected test mutation was treated as authoritative work")
	}
}

func TestCompatibilityClosureDoesNotReduceNormalCapacity(t *testing.T) {
	write := &scriptedTool{name: "write_file"}
	tests := &scriptedTool{name: "str_replace"}
	verify := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(write)
	reg.Register(tests)
	reg.Register(verify)
	a, requests, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("write_file", `{"path":"calc.go"}`)
		case 2:
			return toolSSE("str_replace", `{"path":"calc_test.go"}`)
		case 3:
			return textSSE("implementation and tests are complete")
		default:
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		}
	}, reg)
	defer closeServer()
	a.EnableCompatibilityMode()
	if err := a.RunUserMessage(context.Background(), "change calc.go, add tests, and run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(write.calls) != 1 || len(tests.calls) != 1 || len(verify.calls) != 1 {
		t.Fatalf("normal work was blocked before closure: writes=%d tests=%d verify=%d", len(write.calls), len(tests.calls), len(verify.calls))
	}
	if len(*requests) < 4 || a.RunState.ToolCallsUsed > a.MaxToolCalls || a.RunState.Iterations > a.MaxIterations || a.RunState.Retries > a.MaxRetries {
		t.Fatalf("compatibility budgets changed or were exceeded: requests=%d state=%+v", len(*requests), a.RunState)
	}
}

func TestCompatibilityDoesNotReserveWhenVerificationIsAlreadySatisfied(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("run go test ./...")
	a.RunState.recordVerification("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if reserve := a.compatibilityReserve(10); reserve != 0 {
		t.Fatalf("reserve=%d with authoritative verification", reserve)
	}
}

func TestCompatibilityClosureFailsClosedWithoutLegalVerificationAction(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("run go test ./...")
	a.compatibilityVerificationReserve = 2
	a.beginCompatibilityClosure(func(Event) {})
	events := []Event{}
	a.finishCompatibilityClosure(func(e Event) { events = append(events, e) })
	if a.RunState.Phase != PhaseBlocked || len(events) != 2 || events[0].Kind != EventError {
		t.Fatalf("closure did not fail closed: phase=%s events=%#v", a.RunState.Phase, events)
	}
}

func TestCompatibilityClosureUsesAuthoritativeEvidenceOnly(t *testing.T) {
	a := &Agent{CompatibilityMode: true}
	a.RunState.reset("fix calc.go, add tests, and run go test ./...")
	a.RunState.Verification = VerificationPassed
	a.compatibilityVerificationReserve = 2
	a.beginCompatibilityClosure(func(Event) {})
	if a.compatibilityCanComplete() {
		t.Fatal("model verification claim consumed closure reserve")
	}
	a.RunState.addObservation("write_file", `{"path":"calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "ok"})
	if a.compatibilityCanComplete() {
		t.Fatal("full-suite evidence alone satisfied missing implementation? test requirement")
	}
}

func TestCompatibilityClosureAllowsExistingVerificationPolicyToolsOnly(t *testing.T) {
	all := []llm.Tool{
		{Function: llm.ToolFunctionSchema{Name: "write_file"}},
		{Function: llm.ToolFunctionSchema{Name: "read_file"}},
		{Function: llm.ToolFunctionSchema{Name: "run_tests"}},
		{Function: llm.ToolFunctionSchema{Name: "run_shell"}},
	}
	got := compatibilityVerificationTools(all)
	if len(got) != 2 || got[0].Function.Name != "run_tests" || got[1].Function.Name != "run_shell" {
		t.Fatalf("verification tool filter=%v", got)
	}
	if compatibilityVerificationAction("run_shell", `{"command":"rm -f output.txt"}`) {
		t.Fatal("closure authorized a non-verification shell command")
	}
	if !compatibilityVerificationAction("run_shell", `{"command":"go test ./..."}`) {
		t.Fatal("closure rejected a legal test command")
	}
}

func TestCompatibilityMalformedTextualPseudoCallRemainsUnexecuted(t *testing.T) {
	runner := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(runner)
	a, _, closeServer := testAgent(t, func(int) string {
		return textSSE(`<function=run_tests>{"command":"go test ./..."}</function>`)
	}, reg)
	defer closeServer()
	a.EnableCompatibilityMode()
	if err := a.RunUserMessage(context.Background(), "run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || a.RunState.Phase != PhaseBlocked {
		t.Fatalf("malformed pseudo-call was executed or completed: calls=%d phase=%s", len(runner.calls), a.RunState.Phase)
	}
}

func TestCompatibilityClosureProvidesReservedFinalVerificationTurn(t *testing.T) {
	checker := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(checker)
	a, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return textSSE("all done")
		}
		return toolSSE("run_tests", `{"command":"go test ./..."}`)
	}, reg)
	defer closeServer()
	a.EnableCompatibilityMode()
	if err := a.RunUserMessage(context.Background(), "run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(checker.calls) != 1 || a.RunState.Phase != PhaseComplete || len(*requests) != 2 {
		t.Fatalf("reserved closure did not complete: calls=%d phase=%s requests=%d state=%+v", len(checker.calls), a.RunState.Phase, len(*requests), a.RunState)
	}
	if a.RunState.ToolCallsUsed > 2 || a.RunState.Iterations > a.MaxIterations {
		t.Fatalf("closure exceeded unchanged bounds: %+v", a.RunState)
	}
}
