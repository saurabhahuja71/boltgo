package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// repoMap builds a compact project tree (Grok/Cursor-style repo overview).
type repoMap struct{ Workspace string }

func (repoMap) Name() string { return "repo_map" }
func (repoMap) Description() string {
	return "Return a compact directory tree of the project (symbols-free map). " +
		"Use at the start of unfamiliar repos instead of many list_dir calls. " +
		"Skips .git, node_modules, vendor, etc."
}
func (repoMap) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Root to map (default .)",
			},
			"max_depth": map[string]any{
				"type":        "integer",
				"description": "Max directory depth (default 3, max 5)",
			},
			"max_entries": map[string]any{
				"type":        "integer",
				"description": "Max lines in output (default 200)",
			},
		},
	}
}

func (r repoMap) Run(_ context.Context, argsJSON string) (string, error) {
	var in struct {
		Path       string `json:"path"`
		MaxDepth   int    `json:"max_depth"`
		MaxEntries int    `json:"max_entries"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &in)
	root := in.Path
	if root == "" {
		root = "."
	}
	if in.MaxDepth <= 0 {
		in.MaxDepth = 3
	}
	if in.MaxDepth > 5 {
		in.MaxDepth = 5
	}
	if in.MaxEntries <= 0 {
		in.MaxEntries = 200
	}
	if in.MaxEntries > 500 {
		in.MaxEntries = 500
	}

	root = filepath.Clean(resolveWorkspacePath(r.Workspace, root))
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", root)
	}

	var lines []string
	lines = append(lines, root+"/")
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if len(lines) >= in.MaxEntries {
			return fmt.Errorf("cap")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		// dirs first, then files; sorted
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if len(lines) >= in.MaxEntries {
				return fmt.Errorf("cap")
			}
			name := e.Name()
			if shouldSkipMapName(name) {
				continue
			}
			rel, _ := filepath.Rel(root, filepath.Join(dir, name))
			indent := strings.Repeat("  ", depth)
			if e.IsDir() {
				lines = append(lines, fmt.Sprintf("%s%s/", indent, name))
				if depth+1 < in.MaxDepth {
					_ = walk(filepath.Join(dir, name), depth+1)
				}
			} else {
				lines = append(lines, fmt.Sprintf("%s%s", indent, name))
			}
			_ = rel
		}
		return nil
	}
	_ = walk(root, 1)
	if len(lines) >= in.MaxEntries {
		lines = append(lines, "…[truncated repo_map]…")
	}
	return strings.Join(lines, "\n"), nil
}

func shouldSkipMapName(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "target", ".idea", ".cache",
		"__pycache__", ".venv", "venv", "bin", "pkg", ".terraform", "coverage":
		return true
	}
	if strings.HasPrefix(name, ".") && name != ".github" && name != ".agenterm" {
		// keep .github; skip other dot dirs/files for map noise
		if name == ".gitignore" || name == ".env.example" {
			return false
		}
		return true
	}
	return false
}
