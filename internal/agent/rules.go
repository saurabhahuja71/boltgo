package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// loadProjectRules reads workspace-local AGENTS.md, .agenterm/rules,
// CLAUDE.md (first found, capped). It must not inspect the Bolt source/cwd.
func loadProjectRules(workspace string) string {
	candidates := []string{
		"AGENTS.md",
		".agenterm/rules",
		".agenterm/rules.md",
		"CLAUDE.md",
		".cursorrules",
	}
	// walk up from cwd a few levels for monorepos
	cwd, err := filepath.Abs(workspace)
	if err != nil || cwd == "" {
		return ""
	}
	dir := cwd
	for i := 0; i < 5; i++ {
		for _, name := range candidates {
			p := filepath.Join(dir, name)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			s := strings.TrimSpace(string(data))
			if s == "" {
				continue
			}
			const max = 12_000
			if len(s) > max {
				s = s[:max] + "\n…[truncated project rules]…"
			}
			return "Project rules (" + p + "):\n" + s
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// findRepoRoot walks up for .git or go.mod.
func findRepoRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return findRepoRootAt(cwd)
}

func findRepoRootAt(cwd string) string {
	dir := cwd
	for i := 0; i < 12; i++ {
		if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && st.IsDir() {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd
}
