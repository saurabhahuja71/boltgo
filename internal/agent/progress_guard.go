package agent

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/llm"
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

func diagnosisSynthesisFallback() string {
	return "Diagnosis stopped after repeated searches produced no new evidence. Report any SKIP/HOLD reason codes already observed (for example SKIP_ASSIGNMENT_NOT_REQUIRED), which files were inspected, and that a concrete code fix was not proven. Do not rewrite workflow YAML without evidence."
}

func extractDiagnosisSymbol(user string) string {
	if token := extractAddItemToken(user); token != "" {
		return strings.ToUpper(token)
	}
	low := strings.ToLower(user)
	// Prefer explicit "for SYMBOL stock/symbol" phrasing over any ticker substring.
	if m := regexp.MustCompile(`(?i)\b(?:for|on|about)\s+([a-z][a-z0-9_.-]{1,15})\s+(?:stock|symbol|underlying)\b`).FindStringSubmatch(low); len(m) == 2 {
		return strings.ToUpper(m[1])
	}
	for _, candidate := range []string{"mankind", "bel", "bpcl"} {
		if strings.Contains(low, candidate) {
			return strings.ToUpper(candidate)
		}
	}
	return ""
}

func decisionCodesForSymbol(summary, token string) []string {
	codes := []string{
		"SKIP_ASSIGNMENT_NOT_REQUIRED", "SKIP_NO_CASH", "SKIP_DUPLICATE",
		"SKIP_NO_SUPPORT", "SKIP_ENTRY_LOCKED", "SKIP_NO_OPPORTUNITY", "HOLD",
	}
	var out []string
	seen := map[string]bool{}
	upperToken := strings.ToUpper(strings.TrimSpace(token))
	if upperToken == "" {
		for _, code := range codes {
			if strings.Contains(summary, code) {
				out = append(out, code)
			}
		}
		return out
	}
	// When records are newline-delimited, restrict code extraction to the
	// actual symbol record. The fallback window is only for compacted legacy
	// observations that no longer retain record boundaries.
	var symbolLines []string
	for _, line := range strings.Split(summary, "\n") {
		if strings.Contains(strings.ToUpper(line), upperToken) {
			symbolLines = append(symbolLines, line)
		}
	}
	if len(symbolLines) > 0 {
		for _, line := range symbolLines {
			upperLine := strings.ToUpper(line)
			for _, code := range codes {
				if strings.Contains(upperLine, code) && !seen[code] {
					seen[code] = true
					out = append(out, code)
				}
			}
		}
		return out
	}
	// Observation summaries are often newline-flattened by compactStateText, so
	// a whole decision_log dump becomes one blob with many symbols. Only keep
	// codes that appear near the asked symbol token.
	upper := strings.ToUpper(summary)
	for start := 0; start < len(upper); {
		rel := strings.Index(upper[start:], upperToken)
		if rel < 0 {
			break
		}
		abs := start + rel
		from := abs - 48
		if from < 0 {
			from = 0
		}
		// Keep the complete symbol-scoped record. CSV rows and shell output often
		// put the reason after the symbol, beyond the old 180-byte window.
		to := abs + len(upperToken) + 900
		if to > len(summary) {
			to = len(summary)
		}
		window := summary[from:to]
		for _, code := range codes {
			if strings.Contains(window, code) && !seen[code] {
				seen[code] = true
				out = append(out, code)
			}
		}
		start = abs + len(upperToken)
	}
	return out
}

func diagnosisEvidenceForSymbol(summary, token string) []string {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	// Prefer complete newline-delimited records. This prevents evidence for BEL
	// immediately before MANKIND from being attributed to MANKIND.
	var lineEvidence []string
	seenLines := map[string]bool{}
	for _, line := range strings.Split(summary, "\n") {
		if !strings.Contains(strings.ToUpper(line), strings.ToUpper(token)) {
			continue
		}
		line = strings.Join(strings.Fields(line), " ")
		if line != "" && !seenLines[line] {
			seenLines[line] = true
			lineEvidence = append(lineEvidence, line)
		}
		if len(lineEvidence) == 4 {
			return lineEvidence
		}
	}
	if len(lineEvidence) > 0 {
		// Workflow metadata is often emitted on the line immediately before or
		// after the symbol decision. Include only metadata-shaped lines, never
		// another symbol's decision record.
		for _, line := range strings.Split(summary, "\n") {
			low := strings.ToLower(line)
			if strings.Contains(low, "run_id=") || strings.Contains(low, "workflow=") || strings.Contains(low, "job=") || strings.Contains(low, "schedule=") {
				line = strings.Join(strings.Fields(line), " ")
				if line != "" && !seenLines[line] {
					seenLines[line] = true
					lineEvidence = append(lineEvidence, line)
				}
			}
		}
		return lineEvidence
	}
	upper := strings.ToUpper(summary)
	want := strings.ToUpper(token)
	var evidence []string
	seen := map[string]bool{}
	for start := 0; start < len(upper); {
		rel := strings.Index(upper[start:], want)
		if rel < 0 {
			break
		}
		abs := start + rel
		from := abs - 180
		if from < 0 {
			from = 0
		}
		to := abs + len(want) + 900
		if to > len(summary) {
			to = len(summary)
		}
		snippet := strings.Join(strings.Fields(summary[from:to]), " ")
		if len(snippet) > 360 {
			snippet = snippet[:360] + "…"
		}
		if snippet != "" && !seen[snippet] {
			seen[snippet] = true
			evidence = append(evidence, snippet)
		}
		start = abs + len(want)
	}
	return evidence
}

func diagnosisSynthesisFromState(user string, state AgentRunState) string {
	token := extractDiagnosisSymbol(user)
	var reasons []string
	var evidence []string
	var functions []string
	var files []string
	sourceAuditRead := false
	seenReason := map[string]bool{}
	seenFile := map[string]bool{}
	eqNone := false
	ceNone := false
	for _, obs := range state.Observations {
		summary := obs.Summary
		for _, code := range decisionCodesForSymbol(summary, token) {
			if !seenReason[code] {
				seenReason[code] = true
				reasons = append(reasons, code)
			}
		}
		for _, snippet := range diagnosisEvidenceForSymbol(summary, token) {
			if len(evidence) < 4 {
				evidence = append(evidence, snippet)
			}
		}
		upper := strings.ToUpper(summary)
		if token != "" && strings.Contains(upper, token+" EQ: NONE") {
			eqNone = true
		}
		if token != "" && strings.Contains(upper, token+" CE: NONE") {
			ceNone = true
		}
		for _, function := range diagnosisFunctions(summary) {
			found := false
			for _, existing := range functions {
				if existing == function {
					found = true
					break
				}
			}
			if !found && len(functions) < 4 {
				functions = append(functions, function)
			}
		}
	}
	for _, call := range state.ToolCalls {
		arg := call.Arguments
		if strings.Contains(arg, "decision_audit.py") {
			sourceAuditRead = true
		}
		for _, marker := range []string{"decision_log.csv", "underlyings.txt", "bot.py", "delivery_strategy.py", "decision_audit.py", "rocket_", ".github/workflows/bot.yml"} {
			if strings.Contains(arg, marker) && !seenFile[marker] {
				seenFile[marker] = true
				files = append(files, marker)
			}
		}
	}
	if sourceAuditRead && seenReason["SKIP_ASSIGNMENT_NOT_REQUIRED"] {
		found := false
		for _, function := range functions {
			if function == "resolve_exit_action_and_reason" {
				found = true
				break
			}
		}
		if !found {
			functions = append([]string{"resolve_exit_action_and_reason"}, functions...)
		}
	}
	var b strings.Builder
	b.WriteString("Diagnosis summary from collected tool evidence:\n")
	if token != "" {
		b.WriteString("- symbol: " + token + "\n")
	}
	if len(reasons) > 0 {
		b.WriteString("- observed decision codes for " + token + ": " + strings.Join(reasons, ", ") + "\n")
	} else if token != "" {
		b.WriteString("- observed decision codes for " + token + ": none extracted on symbol-scoped lines\n")
	} else {
		b.WriteString("- observed decision codes: none extracted from tool output\n")
	}
	if eqNone || ceNone {
		b.WriteString("- position evidence:")
		if eqNone {
			b.WriteString(" " + token + " EQ: none;")
		}
		if ceNone {
			b.WriteString(" " + token + " CE: none;")
		}
		b.WriteString("\n")
	}
	if len(files) > 0 {
		b.WriteString("- files/tools touched: " + strings.Join(files, ", ") + "\n")
	}
	if len(evidence) > 0 {
		b.WriteString("- symbol-scoped evidence:\n")
		for _, snippet := range evidence {
			b.WriteString("  - " + snippet + "\n")
		}
	} else if token != "" {
		b.WriteString("- symbol-scoped evidence: missing; collected observations did not contain a readable " + token + " record\n")
	}
	if len(functions) > 0 {
		b.WriteString("- responsible functions observed: " + strings.Join(functions, ", ") + "\n")
	} else {
		b.WriteString("- responsible functions observed: missing; source mapping was not collected\n")
	}
	if len(reasons) > 0 && diagnosisEvidenceReady(user, state) {
		b.WriteString("- root-cause status: decision, source, and requested workflow evidence collected; no workflow scheduling failure proven\n")
	} else if len(reasons) > 0 {
		b.WriteString("- root-cause status: decision evidence found; source function and workflow outcome still require explicit matching evidence\n")
	} else {
		b.WriteString("- root-cause status: unproven; missing a symbol-scoped decision reason\n")
	}
	if token != "" && seenReason["SKIP_ASSIGNMENT_NOT_REQUIRED"] {
		b.WriteString("- interpretation: SKIP_ASSIGNMENT_NOT_REQUIRED with HOLD means the exit/audit path recorded no assignment action; this is not a workflow schedule miss. Check whether " + token + " had an EQ/FUT cover and whether entry was skipped earlier (cash/support/eligibility).\n")
	}
	b.WriteString("- concrete code fix: not proven in this turn; do not rewrite workflow YAML without evidence.\n")
	return b.String()
}

func diagnosisFunctions(summary string) []string {
	var result []string
	seen := map[string]bool{}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bdef\s+([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`\b(resolve_[A-Za-z0-9_]+)\b`),
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(summary, -1) {
			if len(match) > 1 && !seen[match[1]] {
				seen[match[1]] = true
				result = append(result, match[1])
			}
		}
	}
	return result
}

// diagnosisEvidenceReady reports whether the deterministic fallback has
// enough independent evidence to answer a diagnosis request. Once this is
// true, another model search is not useful: it only risks repeating reads or
// inventing a fix. Workflow requests additionally require workflow evidence.
func diagnosisEvidenceReady(user string, state AgentRunState) bool {
	token := extractDiagnosisSymbol(user)
	if token == "" {
		return false
	}
	lowGoal := strings.ToLower(user)
	decision := false
	source := false
	workflow := !strings.Contains(lowGoal, "workflow") && !strings.Contains(lowGoal, "github")
	for _, obs := range state.Observations {
		if len(decisionCodesForSymbol(obs.Summary, token)) > 0 {
			decision = true
		}
		low := strings.ToLower(obs.Summary)
		if strings.Contains(low, "def resolve_") || strings.Contains(low, "resolve_exit_action_and_reason") || strings.Contains(low, "return \"skip\"") {
			source = true
		}
		if strings.Contains(low, "workflow") || strings.Contains(low, "github actions") || strings.Contains(low, "run:") || strings.Contains(low, "schedule:") {
			workflow = true
		}
	}
	for _, call := range state.ToolCalls {
		low := strings.ToLower(call.Arguments)
		if strings.Contains(low, ".github/workflows") || strings.Contains(low, "bot.yml") {
			workflow = true
		}
	}
	return decision && source && workflow
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
		"diagnose", "debug", "why ", "skip_", "not working", "didn't sell", "did not sell",
		"skip assignment", "missed", "deeply", "root cause",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

func continuationRequest(user string) bool {
	s := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(user))), " ")
	switch s {
	case "proceed", "continue", "go ahead", "keep going", "keep going please",
		"yes proceed", "ok proceed", "please proceed", "continue please",
		"yes continue", "ok continue", "do that", "carry on":
		return true
	default:
		return false
	}
}

func isAgentInternalUserMessage(content string) bool {
	c := strings.TrimSpace(content)
	if c == "" {
		return true
	}
	prefixes := []string{
		"ACTION RECOVERY:", "ADD-ITEM", "GIT PUSH REQUIRED:", "[agenterm]",
		"ADD-ITEM NOTE:", "ADD-ITEM TASK:", "The requested file/git change already succeeded",
		"The requested item is already present", "VERIFICATION COMPLETE",
		"Repeated investigation", "Your previous", "Tools are closed",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// priorUserGoal returns the most recent real user task from history so short
// follow-ups like "proceed" can continue debugging instead of asking for
// clarification.
func priorUserGoal(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != llm.RoleUser {
			continue
		}
		content := strings.TrimSpace(history[i].Content)
		if isAgentInternalUserMessage(content) {
			continue
		}
		// Drop injected execute-now suffixes from earlier turns.
		if idx := strings.Index(content, "\n\n[agenterm]"); idx > 0 {
			content = strings.TrimSpace(content[:idx])
		}
		if content != "" {
			return content
		}
	}
	return ""
}

func diagnosisRecoveryGuidance(user string) string {
	token := extractAddItemToken(user)
	guidance := "DIAGNOSIS RECOVERY: stop broad rediscovery and do not read a large decision log whole. First use grep with the exact symbol"
	if token != "" {
		guidance += " " + token
	} else if symbol := extractDiagnosisSymbol(user); symbol != "" {
		guidance += " " + symbol
	}
	guidance += " in decision_log.csv; then read only the matching rows and grep the exact reason code in the decision-audit/strategy files. Read the workflow file and recent log lines only after the symbol rows are captured. Prefer grep/read_file on .github/workflows/bot.yml, bot.py, common/decision_audit.py, common/delivery_strategy.py, and recent logs. Do not rewrite workflow YAML or other files until a concrete root cause is proven from tool evidence."
	return guidance
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

// diagnosisToolAllowed keeps inspection tools available while still blocking
// sprawling rediscovery and destructive rewrite loops after a stall.
func diagnosisToolAllowed(name string) bool {
	switch name {
	case "read_file", "grep", "run_tests", "str_replace", "git":
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
		(strings.Contains(low, "debug") && strings.Contains(low, "fix")) ||
		(strings.Contains(low, "deeply") && strings.Contains(low, "fix")) ||
		(diagnosisOriented(user) && strings.Contains(low, "fix")) ||
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
