package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestInvestigationFingerprintNormalizesEquivalentArguments(t *testing.T) {
	first, ok := investigationFingerprint("grep", `{"pattern":"TODO","path":"."}`)
	if !ok {
		t.Fatal("grep was not classified as investigation")
	}
	second, ok := investigationFingerprint("grep", `{"path":".","pattern":"TODO"}`)
	if !ok || first != second {
		t.Fatalf("equivalent arguments produced different fingerprints: %q != %q", first, second)
	}
	third, _ := investigationFingerprint("grep", `{"path":".","pattern":"FIXME"}`)
	if first == third {
		t.Fatal("different search patterns were suppressed as equivalent")
	}
	if _, ok := investigationFingerprint("write_file", `{"path":"x"}`); ok {
		t.Fatal("mutation was classified as investigation")
	}
}

func TestRepeatedInvestigationObservationIsActionable(t *testing.T) {
	message := repeatedInvestigationObservation("read_file")
	for _, want := range []string{"read_file", "already executed", "state has not changed", "evidence already collected", "do not repeat", "does not exist"} {
		if !strings.Contains(strings.ToLower(message), strings.ToLower(want)) {
			t.Fatalf("observation missing %q: %s", want, message)
		}
	}
}

func TestWorkspaceGenerationAllowsInvestigationAfterMutation(t *testing.T) {
	key, ok := investigationFingerprint("read_file", `{"path":"a.go"}`)
	if !ok {
		t.Fatal("read_file was not classified as investigation")
	}
	seen := map[string]uint64{key: 0}
	if seen[key] != 0 {
		t.Fatal("initial generation was not recorded")
	}
	if !workspaceMutation("write_file") {
		t.Fatal("write_file was not classified as a workspace mutation")
	}
	seen[key] = 1
	if seen[key] == 0 {
		t.Fatal("changed workspace generation still looked unchanged")
	}
}

func TestActionRequestsKeepToolsAfterRepeatedInvestigation(t *testing.T) {
	if !keepActionToolsAfterNoProgress("implement the worker pool fix") {
		t.Fatal("implementation request did not retain tools")
	}
	if keepActionToolsAfterNoProgress("explain how the worker pool works") {
		t.Fatal("explanation request incorrectly retained action tools")
	}
}

func TestWorkerPoolLifecycleTaskDetection(t *testing.T) {
	if !workerPoolLifecycleTask("diagnose and fix the Go worker-pool implementation") {
		t.Fatal("worker-pool action query was not detected")
	}
	if !workerPoolLifecycleTask("fix the worker pool lifecycle") {
		t.Fatal("worker pool query was not detected")
	}
	if workerPoolLifecycleTask("explain the scheduler") {
		t.Fatal("unrelated query was treated as a worker-pool task")
	}
	processPrompt := "A worker-pool style processor is required:\n\n    func Process(ctx context.Context, jobs <-chan int) ([]int, error)\n\npreserve input order"
	if !orderedChannelProcessTask(processPrompt) {
		t.Fatal("Process channel query was not detected")
	}
	if workerPoolLifecycleTask(processPrompt) {
		t.Fatal("Process channel query was misclassified as read-batch worker-pool task")
	}
}

func TestActionExecutionBudgetRaisesHeavyEngineeringCaps(t *testing.T) {
	rounds, calls, cap := actionExecutionBudget("diagnose and fix a production-safety issue in the existing Go worker-pool implementation\n\ngo test ./...\nCGO_ENABLED=1 go test -race ./...")
	if rounds < 36 || calls < 96 || cap < 96 || cap != calls {
		t.Fatalf("heavy worker-pool budget too small or soft-capped early: rounds=%d calls=%d cap=%d", rounds, calls, cap)
	}
	pr, pc, pcap := actionExecutionBudget("diagnose and fix func Process(ctx context.Context, jobs <-chan int) ([]int, error) and preserve input order")
	if pr < 36 || pc < 96 || pcap != pc {
		t.Fatalf("Process-channel budget too small: rounds=%d calls=%d cap=%d", pr, pc, pcap)
	}
	r2, c2, cap2 := actionExecutionBudget("implement a one-line typo fix")
	if r2 != 12 || c2 != 24 || cap2 != 12 {
		t.Fatalf("normal action budget = rounds=%d calls=%d cap=%d, want 12/24/12", r2, c2, cap2)
	}
	r3, c3, cap3 := actionExecutionBudget("explain the worker pool")
	if r3 != 8 || c3 != 24 || cap3 != 4 {
		t.Fatalf("non-action budget = rounds=%d calls=%d cap=%d, want 8/24/4", r3, c3, cap3)
	}
}

func TestResolveRepoRelativePathFindsWorkerPoolSource(t *testing.T) {
	got := resolveRepoRelativePath("internal/agent/read_batch.go")
	if got == "" {
		t.Fatal("did not resolve worker-pool implementation path from repository root")
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("resolved path %q is not readable: %v", got, err)
	}
}

func TestOrderedChannelProcessRewriteAndRediscoveryGuards(t *testing.T) {
	if !orderedChannelProcessFullRewrite("write_file", `{"path":"internal/worker/process.go","content":"package worker"}`) {
		t.Fatal("full rewrite of process.go was not blocked")
	}
	if orderedChannelProcessFullRewrite("str_replace", `{"path":"internal/worker/process.go"}`) {
		t.Fatal("surgical str_replace was incorrectly blocked")
	}
	if !orderedChannelProcessSourceRead("read_file", `{"path":"internal/worker/process.go"}`) {
		t.Fatal("process source read was not detected")
	}
}

func TestDiscoveryShellCommandsAreFingerprinted(t *testing.T) {
	first, ok := investigationFingerprint("run_shell", `{"command":"ls -la internal/agent/"}`)
	if !ok {
		t.Fatal("discovery shell was not fingerprinted")
	}
	second, ok := investigationFingerprint("run_shell", `{"command":"ls -la internal/agent/"}`)
	if !ok || first != second {
		t.Fatalf("equivalent shell listings produced different fingerprints: %q != %q", first, second)
	}
	if _, ok := investigationFingerprint("run_shell", `{"command":"go test ./..."}`); !ok {
		t.Fatal("identical go test shell commands must be fingerprinted to prevent re-run loops")
	}
	if discoveryShellCommand(`{"command":"go test ./..."}`) {
		t.Fatal("go test shell command was classified as discovery")
	}
	if workspaceMutation("run_shell") || workspaceMutation("run_tests") {
		t.Fatal("verification tools must not reset investigation generations as workspace mutations")
	}
	if !workerPoolRediscoveryTool("run_shell", `{"command":"ls -la internal/agent/"}`) {
		t.Fatal("worker-pool rediscovery did not catch shell listing")
	}
	if workerPoolRediscoveryTool("run_shell", `{"command":"go test ./internal/agent/ -run ReadOnlyBatch"}`) {
		t.Fatal("verification shell was blocked as rediscovery")
	}
}

func TestAddItemActionHelpers(t *testing.T) {
	prompt := "pls add mankind to covered ce strategy and do git push"
	if extractAddItemToken(prompt) != "mankind" {
		t.Fatalf("token=%q", extractAddItemToken(prompt))
	}
	if !addItemActionTask(prompt) || !goalRequestsGitAction(prompt) {
		t.Fatal("add/git task was not detected")
	}
	hint := addItemActionHint(prompt)
	for _, want := range []string{"underlyings.txt", "mankind", "git"} {
		if !strings.Contains(strings.ToLower(hint), strings.ToLower(want)) {
			t.Fatalf("hint missing %q: %s", want, hint)
		}
	}
	recovery := actionRecoveryGuidance(prompt)
	for _, want := range []string{"underlyings.txt", "MANKIND", "git", "repo_map"} {
		// recovery mentions token as provided / upper in sentence; accept either case via lower compare for most
		if want == "MANKIND" {
			if !strings.Contains(recovery, "mankind") && !strings.Contains(recovery, "MANKIND") {
				t.Fatalf("recovery missing token: %s", recovery)
			}
			continue
		}
		if !strings.Contains(strings.ToLower(recovery), strings.ToLower(want)) {
			t.Fatalf("recovery missing %q: %s", want, recovery)
		}
	}
	if actionMutationToolAllowed("repo_map") || actionMutationToolAllowed("list_dir") || actionMutationToolAllowed("find_files") {
		t.Fatal("broad discovery tools incorrectly allowed during action recovery")
	}
	if !actionMutationToolAllowed("str_replace") || !actionMutationToolAllowed("git") || !actionMutationToolAllowed("read_file") || !actionMutationToolAllowed("grep") {
		t.Fatal("edit/git/diagnose tools were blocked during action recovery")
	}
}

func TestAddItemPushOnlyGoalVsDiagnosis(t *testing.T) {
	pushOnly := "pls add mankind to covered ce strategy and do git push"
	diag := "github workflow runs today show no ce sold for mankind; diagnose why"
	if !addItemPushOnlyGoal(pushOnly) {
		t.Fatal("push-only add-item goal not detected")
	}
	if addItemPushOnlyGoal(diag) {
		t.Fatal("diagnosis goal incorrectly treated as push-only")
	}
	if !diagnosisOriented(diag) || diagnosisOriented(pushOnly) {
		t.Fatal("diagnosisOriented mismatch")
	}
	hint := addItemActionHint(pushOnly)
	if strings.Contains(strings.ToLower(hint), "decision_log") {
		t.Fatalf("push-only hint should not force decision_log tour: %s", hint)
	}
}

func TestNaturalMankindDiagnosisGetsBoundedSynthesis(t *testing.T) {
	query := "check why mankind ce dont sell today ? https://github.com/Tradebots71/covered_call_bot/actions/workflows/bot.yml"
	if !diagnosisOriented(query) || !isActionRequest(query) {
		t.Fatalf("natural-language workflow diagnosis was not classified: diagnosis=%v action=%v", diagnosisOriented(query), isActionRequest(query))
	}
	if rounds, _, _ := actionExecutionBudget(query); rounds < 8 {
		t.Fatalf("natural-language diagnosis budget too small: %d", rounds)
	}
	text := diagnosisSynthesisInstruction()
	for _, want := range []string{"stop calling tools", "SKIP/HOLD", "missing evidence", "workflow YAML"} {
		if !strings.Contains(text, want) {
			t.Fatalf("synthesis instruction missing %q: %s", want, text)
		}
	}
}

func TestDiagnosisRecoveryPrioritizesCoveredCallEvidence(t *testing.T) {
	guidance := diagnosisRecoveryGuidance("check why mankind ce dont sell today ? https://github.com/Tradebots71/covered_call_bot/actions/workflows/bot.yml")
	for _, want := range []string{"decision_log.csv", "common/decision_audit.py", "monitor/data/intraday/eod_*.json", "logs/bot_*.log"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("diagnosis guidance missing %q: %s", want, guidance)
		}
	}
}

func TestContinuationAndDiagnosisHelpers(t *testing.T) {
	if !continuationRequest("proceed") || !continuationRequest("keep going") {
		t.Fatal("continuationRequest missed proceed/keep going")
	}
	if continuationRequest("pls add mankind to covered ce strategy") {
		t.Fatal("normal request treated as continuation")
	}
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "pls add mankind to covered ce strategy and do git push"},
		{Role: llm.RoleAssistant, Content: "done"},
		{Role: llm.RoleUser, Content: "its not about schedule why mankind was not sold in all scheduled run we need to fix it after debugging it deeply"},
		{Role: llm.RoleAssistant, Content: "ok"},
		{Role: llm.RoleUser, Content: "ACTION RECOVERY: discovery tools are disabled"},
	}
	prior := priorUserGoal(history)
	if !strings.Contains(strings.ToLower(prior), "not sold") {
		t.Fatalf("priorUserGoal=%q, want diagnosis goal", prior)
	}
	if !diagnosisOriented(prior) {
		t.Fatal("prior diagnosis goal not detected")
	}
	recovery := diagnosisRecoveryGuidance("this is for mankind stock why all jobs missed it today")
	for _, want := range []string{"exact symbol", "MANKIND", "decision_log.csv", "workflow file", "matching rows", "do not read a large decision log whole"} {
		if !strings.Contains(strings.ToLower(recovery), strings.ToLower(want)) {
			t.Fatalf("diagnosis recovery missing %q: %s", want, recovery)
		}
	}
	if diagnosisToolAllowed("repo_map") || diagnosisToolAllowed("list_dir") || diagnosisToolAllowed("write_file") || diagnosisToolAllowed("find_files") || diagnosisToolAllowed("run_shell") {
		t.Fatal("diagnosis recovery allowed unsafe rediscovery/write/shell tools")
	}
	if !diagnosisToolAllowed("read_file") || !diagnosisToolAllowed("grep") || !diagnosisToolAllowed("str_replace") {
		t.Fatal("diagnosis recovery blocked needed inspect/fix tools")
	}
}

func TestDiagnosisSynthesisScopesCodesToSymbol(t *testing.T) {
	var state AgentRunState
	state.reset("this is for mankind stock why all jobs missed it today")
	state.addObservation("read_file", `{"path":"decision_log.csv"}`, tools.ExecutionResult{
		Output: strings.Join([]string{
			"2026-09-24T10:00:00+05:30,BEL,SKIP,SKIP_NO_CASH,weak,,,,,,,,,,,,False,False,True,,,DELIVERY",
			"2026-09-24T10:00:01+05:30,BEL,SKIP,SKIP_NO_SUPPORT,,,,,,,,,,,,False,False,True,,,DELIVERY",
			"2026-09-24T10:31:47+05:30,MANKIND,SKIP,SKIP_ASSIGNMENT_NOT_REQUIRED,HOLD,,,,,,,,,,,,False,False,True,,,STANDARD",
			"2026-09-24T12:01:38+05:30,MANKIND,SKIP,SKIP_ASSIGNMENT_NOT_REQUIRED,HOLD,,,,,,,,,,,,False,False,True,,,STANDARD",
		}, "\n"),
		Category: tools.FailureSuccess,
	})
	state.addObservation("read_file", `{"path":"logs/rocket_2026-09-24.log"}`, tools.ExecutionResult{
		Output:   "ACTIVE UNDERLYINGS: ['BEL', 'MANKIND']\n  MANKIND EQ: none\n  MANKIND CE: none\n",
		Category: tools.FailureSuccess,
	})
	out := diagnosisSynthesisFromState(state.OriginalGoal, state)
	if !strings.Contains(out, "SKIP_ASSIGNMENT_NOT_REQUIRED") {
		t.Fatalf("missing MANKIND code: %s", out)
	}
	if strings.Contains(out, "SKIP_NO_CASH") || strings.Contains(out, "SKIP_NO_SUPPORT") {
		t.Fatalf("attributed BEL codes to MANKIND: %s", out)
	}
	if !strings.Contains(out, "MANKIND EQ: none") || !strings.Contains(out, "MANKIND CE: none") {
		t.Fatalf("missing position evidence: %s", out)
	}
}

func TestDiagnosisSynthesisComplexQueries(t *testing.T) {
	tests := []struct {
		name       string
		goal       string
		output     string
		want       []string
		mustNot    []string
		wantStatus string
	}{
		{
			name: "long csv record keeps paired reason",
			goal: "this is for mankind stock why all jobs missed it today",
			output: "header,header,header\n" +
				"2026-09-24T12:01:38+05:30,MANKIND,SKIP,SKIP_ASSIGNMENT_NOT_REQUIRED,HOLD,entry=false,cover=false,delivery=STANDARD",
			want: []string{"SKIP_ASSIGNMENT_NOT_REQUIRED", "symbol-scoped evidence", "MANKIND,SKIP"},
		},
		{
			name:    "mixed symbols stay isolated",
			goal:    "diagnose why MANKIND was missed in every workflow run",
			output:  "BEL,SKIP,SKIP_NO_CASH\nMANKIND,SKIP,SKIP_ASSIGNMENT_NOT_REQUIRED,HOLD\nBEL,SKIP,SKIP_NO_SUPPORT",
			want:    []string{"SKIP_ASSIGNMENT_NOT_REQUIRED", "MANKIND,SKIP"},
			mustNot: []string{"SKIP_NO_CASH", "SKIP_NO_SUPPORT"},
		},
		{
			name:   "free form hold reason is visible",
			goal:   "deeply debug the MANKIND HOLD reason and responsible function",
			output: "MANKIND decision=HOLD reason=NO_OPEN_CE_POSITION function=delivery_strategy.select_exit_candidate",
			want:   []string{"HOLD", "NO_OPEN_CE_POSITION", "delivery_strategy.select_exit_candidate"},
		},
		{
			name:   "workflow evidence is visible",
			goal:   "inspect GitHub workflow logs and explain why MANKIND was missed",
			output: "run_id=99127 job=covered_call status=success\nMANKIND decision=HOLD source=bot.py:418\nworkflow=bot.yml schedule=12:01+05:30",
			want:   []string{"run_id=99127", "bot.py:418", "workflow=bot.yml"},
		},
		{
			name:       "missing symbol evidence stays explicit",
			goal:       "why did MANKIND miss today; diagnose from the collected logs",
			output:     "BEL,SKIP,SKIP_NO_CASH\nworkflow run completed successfully",
			want:       []string{"symbol-scoped evidence: missing", "root-cause status: unproven"},
			mustNot:    []string{"BEL,SKIP"},
			wantStatus: "unproven",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state AgentRunState
			state.reset(tt.goal)
			state.addObservation("read_file", `{"path":"decision_log.csv"}`, tools.ExecutionResult{Output: tt.output, Category: tools.FailureSuccess})
			got := diagnosisSynthesisFromState(tt.goal, state)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in diagnosis:\n%s", want, got)
				}
			}
			for _, unwanted := range tt.mustNot {
				if strings.Contains(got, unwanted) {
					t.Fatalf("cross-symbol or unsupported evidence %q in diagnosis:\n%s", unwanted, got)
				}
			}
			if tt.wantStatus != "" && !strings.Contains(got, "root-cause status: "+tt.wantStatus) {
				t.Fatalf("missing status %q in diagnosis:\n%s", tt.wantStatus, got)
			}
		})
	}
}

func TestDiagnosisEvidenceReadyRequiresIndependentEvidence(t *testing.T) {
	var state AgentRunState
	state.reset("inspect GitHub workflow logs and explain why MANKIND was missed")
	state.addObservation("grep", `{"pattern":"MANKIND","path":"decision_log.csv"}`, tools.ExecutionResult{
		Output:   "2026-09-24,MANKIND,SKIP,SKIP_ASSIGNMENT_NOT_REQUIRED,HOLD",
		Category: tools.FailureSuccess,
	})
	if diagnosisEvidenceReady(state.OriginalGoal, state) {
		t.Fatal("decision-only evidence incorrectly completed diagnosis")
	}
	state.addObservation("grep", `{"pattern":"SKIP_ASSIGNMENT_NOT_REQUIRED","path":"common/decision_audit.py"}`, tools.ExecutionResult{
		Output:   `365: return "SKIP", "SKIP_ASSIGNMENT_NOT_REQUIRED", exit_type or "HOLD"`,
		Category: tools.FailureSuccess,
	})
	state.addObservation("read_file", `{"path":".github/workflows/bot.yml"}`, tools.ExecutionResult{
		Output:   "name: bot\n schedule: 31 10 * * 1-5",
		Category: tools.FailureSuccess,
	})
	if !diagnosisEvidenceReady(state.OriginalGoal, state) {
		t.Fatal("complete decision/source/workflow evidence was not recognized")
	}
}

func TestCompactDiagnosisTextRetainsDecisionFunction(t *testing.T) {
	input := strings.Repeat("unrelated source line\n", 100) +
		"def resolve_exit_action_and_reason(ctx):\n" +
		"    return \"SKIP\", \"SKIP_ASSIGNMENT_NOT_REQUIRED\", \"HOLD\"\n" +
		strings.Repeat("trailing source line\n", 100)
	got := compactDiagnosisText(input, 1200)
	for _, want := range []string{"resolve_exit_action_and_reason", "SKIP_ASSIGNMENT_NOT_REQUIRED", "return \"SKIP\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic compaction dropped %q:\n%s", want, got)
		}
	}
}
