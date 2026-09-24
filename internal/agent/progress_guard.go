package agent

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// investigationFingerprint identifies read-only investigation actions whose
// result can be compared across model rounds. Canonical JSON makes argument
// key ordering irrelevant while retaining paths, patterns, and other inputs.
func investigationFingerprint(tool, args string) (string, bool) {
	switch tool {
	case "read_file", "find_files", "grep", "list_dir", "repo_map", "run_shell", "run_tests":
	default:
		return "", false
	}
	var value any
	if err := json.Unmarshal([]byte(args), &value); err == nil {
		canonical, err := json.Marshal(value)
		if err == nil {
			return tool + "\x00" + string(canonical), true
		}
	}
	return tool + "\x00" + strings.Join(strings.Fields(args), " "), true
}

func workspaceMutation(tool string) bool {
	switch tool {
	case "write_file", "str_replace", "git":
		return true
	default:
		return false
	}
}

// discoveryShellCommand reports whether a run_shell payload is repository
// rediscovery (ls/find/grep/cat) rather than build/test verification.
func discoveryShellCommand(args string) bool {
	cmd := strings.ToLower(args)
	if strings.Contains(cmd, "go test") || strings.Contains(cmd, "go build") ||
		strings.Contains(cmd, "go vet") || strings.Contains(cmd, "make ") {
		return false
	}
	return true
}

func workerPoolRediscoveryTool(tool, args string) bool {
	switch tool {
	case "list_dir", "grep", "repo_map", "find_files":
		return true
	case "run_shell":
		return discoveryShellCommand(args)
	default:
		return false
	}
}

func workerPoolSourceRead(tool, args string) bool {
	if tool != "read_file" {
		return false
	}
	low := strings.ToLower(args)
	return strings.Contains(low, "read_batch.go")
}

func repeatedInvestigationObservation(tool string) string {
	return "no progress: " + tool + " was already executed with the same arguments and the relevant workspace state has not changed. Use the evidence already collected; do not repeat the same search or read. If the requested implementation does not exist, explicitly report that fact."
}

func synthesisPlanningText(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" || strings.Contains(low, "<tool_call>") {
		return true
	}
	for _, prefix := range []string{"i need to ", "let me ", "i'll ", "i will ", "first, let", "based on the request"} {
		if strings.HasPrefix(low, prefix) && (strings.Contains(low, "inspect") || strings.Contains(low, "search") || strings.Contains(low, "find") || strings.Contains(low, "tool")) {
			return true
		}
	}
	return false
}

func keepActionToolsAfterNoProgress(user string) bool {
	return isActionRequest(user)
}

func noProgressSynthesisFallback() string {
	return "Investigation stopped after repeated searches produced no new evidence. No workspace mutation was performed, and no worker-pool implementation was located in the searched repository evidence. No fix was applied."
}

var addItemToPattern = regexp.MustCompile(`(?i)\badd\s+([A-Za-z0-9_./-]+)\s+to\b`)

func extractAddItemToken(user string) string {
	match := addItemToPattern.FindStringSubmatch(user)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func addItemActionTask(user string) bool {
	return extractAddItemToken(user) != ""
}

func goalRequestsGitAction(user string) bool {
	low := strings.ToLower(user)
	return strings.Contains(low, "git push") ||
		strings.Contains(low, "git commit") ||
		strings.Contains(low, "commit ") ||
		strings.Contains(low, " and push") ||
		strings.Contains(low, "do git")
}

func diagnosisOriented(user string) bool {
	low := strings.ToLower(user)
	for _, needle := range []string{
		"workflow", "github action", "not sold", "no ce", "ce sold", "decision_log",
		"diagnose", "why ", "skip_", "not working", "didn't sell", "did not sell",
		"skip assignment", "hold",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// addItemPushOnlyGoal is the narrow "add X … and git push" request without an
// explicit diagnosis ask. Those should finish after list update/push, not after
// a long decision_log tour.
func addItemPushOnlyGoal(user string) bool {
	return addItemActionTask(user) && goalRequestsGitAction(user) && !diagnosisOriented(user)
}

// actionMutationToolAllowed is the post-recovery allow-list. Discovery tools are
// excluded so stalled add/edit tasks cannot burn the remaining round budget on
// repo_map/list_dir/grep loops.
func actionMutationToolAllowed(name string) bool {
	switch name {
	case "str_replace", "write_file", "git", "read_file", "grep":
		return true
	default:
		return false
	}
}

func addItemActionHint(user string) string {
	token := extractAddItemToken(user)
	if token == "" {
		return ""
	}
	hint := "ADD-ITEM TASK: read underlyings.txt first (also check similar list/config *.txt files if needed). "
	if diagnosisOriented(user) {
		hint += "If " + token + " is already present, inspect decision_log.csv and workflow/bot config for SKIP/HOLD/no-CE reasons"
		if goalRequestsGitAction(user) {
			hint += ", then git commit/push only if needed"
		}
		hint += ". Prefer underlyings.txt and decision_log.csv over README exploration."
		return hint
	}
	hint += "If " + token + " is already present"
	if goalRequestsGitAction(user) {
		hint += ", call git push next (or report already up to date)"
	} else {
		hint += ", confirm no file change is required"
	}
	hint += ". Otherwise add it with str_replace or write_file"
	if goalRequestsGitAction(user) {
		hint += ", then git add/commit/push"
	}
	hint += ". Prefer underlyings.txt over README exploration."
	return hint
}

func actionRecoveryGuidance(user string) string {
	if token := extractAddItemToken(user); token != "" {
		guidance := "ACTION RECOVERY: discovery tools are disabled. Read underlyings.txt if you have not already. "
		guidance += "If " + token + " is already present"
		if goalRequestsGitAction(user) {
			guidance += ", call git push now"
		} else if diagnosisOriented(user) {
			guidance += ", inspect decision_log.csv for SKIP/HOLD reasons"
		} else {
			guidance += ", confirm that no file change is required"
		}
		guidance += ". Otherwise edit underlyings.txt with str_replace/write_file"
		if goalRequestsGitAction(user) {
			guidance += " and then git add/commit/push"
		}
		guidance += ". Do not call repo_map, list_dir, or find_files again."
		return guidance
	}
	guidance := "ACTION RECOVERY: discovery tools are disabled. Use evidence already collected and apply the requested change with str_replace or write_file on the relevant path"
	if goalRequestsGitAction(user) {
		guidance += ", then git add/commit/push"
	}
	guidance += ". Do not repeat grep, repo_map, list_dir, find_files, or the same read_file."
	return guidance
}

// orderedChannelProcessTask identifies the Process(ctx, jobs <-chan int)
// reliability query. It must not be confused with the read-only batch pool.
func orderedChannelProcessTask(user string) bool {
	low := strings.ToLower(user)
	return strings.Contains(low, "jobs <-chan int") ||
		strings.Contains(low, "func process(ctx") ||
		(strings.Contains(low, "preserve input order") && strings.Contains(low, "jobs"))
}

// workerPoolLifecycleTask identifies the production read-batch worker-pool
// reliability query. That task has a single known implementation; bolt must
// not spend its bounded action budget rediscovering the repository layout.
func workerPoolLifecycleTask(user string) bool {
	if orderedChannelProcessTask(user) {
		return false
	}
	low := strings.ToLower(user)
	return strings.Contains(low, "worker-pool") || strings.Contains(low, "worker pool")
}

// actionExecutionBudget returns the per-turn round, tool-call, and soft tool
// caps for an action request. End-to-end diagnose/fix/verify queries need a
// larger budget than short edits; otherwise models hit "bounded autonomy
// limit reached" mid-implementation.
func actionExecutionBudget(user string) (maxRounds, maxToolCalls, toolCap int) {
	maxRounds, maxToolCalls, toolCap = 12, 24, 12
	if !isActionRequest(user) {
		return 8, 24, 4
	}
	low := strings.ToLower(user)
	heavy := workerPoolLifecycleTask(user) ||
		orderedChannelProcessTask(user) ||
		strings.Contains(low, "go test ./...") ||
		strings.Contains(low, "end-to-end") ||
		(strings.Contains(low, "diagnose") && strings.Contains(low, "fix")) ||
		(strings.Contains(low, "race") && strings.Contains(low, "test"))
	if heavy {
		// Inspect + edit + focused tests + full suite + race suite, with room
		// to recover from a failing verification pass. Keep the soft tool cap
		// aligned with maxToolCalls so Bolt does not strip tools mid-run and
		// then reject the model's next tool call as "bounded autonomy limit".
		return 48, 120, 120
	}
	return maxRounds, maxToolCalls, toolCap
}

func workerPoolImplementationPaths() []string {
	return []string{
		"internal/agent/read_batch.go",
		"internal/agent/read_batch_test.go",
	}
}

func orderedChannelProcessPaths() []string {
	return []string{
		"internal/worker/process.go",
		"internal/worker/process_test.go",
	}
}

func orderedChannelProcessGuidance() string {
	return "Ordered channel Process API already lives at internal/worker/process.go (tests: internal/worker/process_test.go). Signature: func Process(ctx context.Context, jobs <-chan int) ([]int, error). Read that file once. Do not rewrite it with write_file unless a real defect exists; prefer surgical str_replace. Do not edit read_batch.go. Do not cd outside the workspace and do not use absolute paths like /scratch/... . Prefer the run_tests tool with command `go test ./internal/worker/ -count=1`, then `go test ./...`, then `CGO_ENABLED=1 go test -race ./...` (run_tests isolates GOCACHE automatically). Then write the final report."
}

func orderedChannelProcessRecoveryGuidance() string {
	return "Stop rediscovery now. Process already exists in internal/worker/process.go with tests in process_test.go. Call run_tests with go test ./internal/worker/ immediately. Do not cd to /scratch or other absolute paths outside the workspace. Only use str_replace if a concrete failing test proves a defect. Never rewrite the whole file. Do not call grep, list_dir, repo_map, find_files, or discovery shell commands again."
}

func orderedChannelProcessRediscoveryObservation() string {
	return "no progress: Process already exists at internal/worker/process.go. Stop rediscovery. Call run_tests / go test ./internal/worker/ now, or use surgical str_replace only if a real defect is proven."
}

func orderedChannelProcessSourceRead(tool, args string) bool {
	if tool != "read_file" {
		return false
	}
	low := strings.ToLower(args)
	return strings.Contains(low, "process.go")
}

func orderedChannelProcessTargetMutation(tool, args string) bool {
	switch tool {
	case "str_replace", "write_file":
	default:
		return false
	}
	low := strings.ToLower(args)
	return strings.Contains(low, "process.go") || strings.Contains(low, "process_test.go")
}

// orderedChannelProcessFullRewrite blocks whole-file write_file replacements of
// an already-read Process implementation. Models otherwise burn the round budget
// rewriting a working file into a broken one.
func orderedChannelProcessFullRewrite(tool, args string) bool {
	if tool != "write_file" {
		return false
	}
	low := strings.ToLower(args)
	return strings.Contains(low, "process.go")
}

func orderedChannelProcessRewriteObservation() string {
	return "no progress: Process already exists. Do not rewrite internal/worker/process.go with write_file. Use surgical str_replace for a proven defect, or call run_tests / go test ./internal/worker/ to verify and finish."
}

func workerPoolActionGuidance() string {
	return "Worker-pool implementation path: internal/agent/read_batch.go (tests: internal/agent/read_batch_test.go). Call read_file on that path first. Keep the existing readOnlyBatchPool / Submit / Close API — use surgical str_replace patches only; never rewrite the file with a new type. Do not search agent.go and do not use shell ls/find/grep for discovery. After edits, run focused pool tests, `GOCACHE=/tmp/boltgo-test-cache go test ./...`, and `CGO_ENABLED=1 GOCACHE=/tmp/boltgo-race-cache go test -race ./...`."
}

func workerPoolRecoveryGuidance() string {
	return "The worker-pool implementation is already in internal/agent/read_batch.go as readOnlyBatchPool. Stop rediscovery. If a defect remains, patch it with str_replace while preserving the existing API. If the implementation already satisfies the lifecycle requirements, add any missing regression tests and run the requested Go tests. Never replace the type with a new WorkerPool/ReadBatch API."
}

func workerPoolRediscoveryObservation() string {
	return "no progress: the worker-pool source was already located at internal/agent/read_batch.go. Do not rediscover the repository. Call str_replace or write_file to implement the lifecycle fix, or run_tests / go test for verification."
}

func workerPoolActionToolAllowed(name string) bool {
	switch name {
	case "str_replace", "write_file", "read_file", "run_tests", "run_shell":
		return true
	default:
		return false
	}
}

// workerPoolTargetMutation is true only when an edit targeted the real pool
// sources. Writes to scratch files must not lift rediscovery protections.
func workerPoolTargetMutation(tool, args string) bool {
	switch tool {
	case "str_replace", "write_file":
	default:
		return false
	}
	low := strings.ToLower(args)
	return strings.Contains(low, "read_batch.go") || strings.Contains(low, "read_batch_test.go")
}

// resolveRepoRelativePath maps a repository-relative path to an absolute path
// when the file exists under the detected project root. Callers can then attach
// the file even when the process cwd is a nested package directory.
func resolveRepoRelativePath(rel string) string {
	rel = strings.TrimPrefix(filepath.Clean(rel), string(filepath.Separator))
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "."
	}
	candidates := []string{
		rel,
		filepath.Join(cwd, rel),
		filepath.Join(findRepoRootAt(cwd), rel),
	}
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}

func investigationEvidenceFingerprint(output string) (string, bool) {
	output = strings.TrimSpace(output)
	if output == "" {
		return "", false
	}
	hash := sha256.Sum256([]byte(output))
	return string(hash[:]), true
}
