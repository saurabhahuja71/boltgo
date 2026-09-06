package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectRulesUsesWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("workspace-only-rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := loadProjectRules(workspace)
	if !strings.Contains(rules, "workspace-only-rule") || !strings.Contains(rules, workspace) {
		t.Fatalf("workspace rules not loaded: %q", rules)
	}
}

func TestLoadProjectRulesMissingWorkspaceIsEmpty(t *testing.T) {
	if got := loadProjectRules(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("rules from outside workspace: %q", got)
	}
}
