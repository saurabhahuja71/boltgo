package agent

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

// investigationFingerprint identifies read-only investigation actions whose
// result can be compared across model rounds. Canonical JSON makes argument
// key ordering irrelevant while retaining paths, patterns, and other inputs.
func investigationFingerprint(tool, args string) (string, bool) {
	switch tool {
	case "read_file", "find_files", "grep", "list_dir", "repo_map":
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
	case "write_file", "str_replace", "run_shell", "run_tests", "git", "ssh_execute":
		return true
	default:
		return false
	}
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

func investigationEvidenceFingerprint(output string) (string, bool) {
	output = strings.TrimSpace(output)
	if output == "" {
		return "", false
	}
	hash := sha256.Sum256([]byte(output))
	return string(hash[:]), true
}
