package agent

import (
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
