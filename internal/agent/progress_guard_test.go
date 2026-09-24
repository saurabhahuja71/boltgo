package agent

import (
	"os"
	"strings"
	"testing"
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
