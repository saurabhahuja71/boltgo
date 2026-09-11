package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// grepTool searches file contents (rg if available, else walk+regexp).
type grepTool struct{ Workspace string }

func (grepTool) Name() string { return "grep" }
func (grepTool) Description() string {
	return "Search file contents for a pattern (regex). Prefer this over reading whole trees. Uses ripgrep (rg) when installed."
}
func (grepTool) Schema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"pattern"},
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Regex or fixed string to search for",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Root file or directory (default .)",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Optional file glob e.g. *.go, *.md",
			},
			"case_insensitive": map[string]any{"type": "boolean"},
			"max_results":      map[string]any{"type": "integer", "description": "Max matches (default 40)"},
		},
	}
}

func (g grepTool) Run(ctx context.Context, argsJSON string) (string, error) {
	var in struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		CaseInsensitive bool   `json:"case_insensitive"`
		MaxResults      int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil || strings.TrimSpace(in.Pattern) == "" {
		return "", fmt.Errorf("pattern required")
	}
	if in.Path == "" {
		in.Path = "."
	}
	path, err := resolveWorkspacePathChecked(g.Workspace, in.Path)
	if err != nil {
		return "", err
	}
	in.Path = path
	if in.MaxResults <= 0 {
		in.MaxResults = 40
	}
	if _, err := exec.LookPath("rg"); err == nil {
		return runRipgrep(ctx, in.Pattern, in.Path, in.Glob, in.CaseInsensitive, in.MaxResults)
	}
	return runWalkGrep(in.Pattern, in.Path, in.Glob, in.CaseInsensitive, in.MaxResults)
}

func runRipgrep(ctx context.Context, pattern, path, glob string, ci bool, max int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--line-number", "--no-heading", "--color", "never", "--max-count", fmt.Sprintf("%d", max)}
	if ci {
		args = append(args, "-i")
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	args = append(args, pattern, path)
	cmd := exec.CommandContext(ctx, "rg", args...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > 30_000 {
		s = s[:30_000] + "\n…[truncated]…"
	}
	// rg exits 1 when no matches
	if err != nil && s == "" {
		return "no matches", nil
	}
	if s == "" {
		return "no matches", nil
	}
	return s, nil
}

func runWalkGrep(pattern, root, glob string, ci bool, max int) (string, error) {
	pat := pattern
	if ci {
		pat = "(?i)" + pattern
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	var b strings.Builder
	n := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || n >= max {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "target", ".idea":
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			ok, _ := filepath.Match(glob, filepath.Base(path))
			if !ok {
				return nil
			}
		}
		// skip large/binary-ish
		st, err := d.Info()
		if err != nil || st.Size() > 2_000_000 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d:%s\n", path, lineNo, truncateLine(line, 200))
				n++
				if n >= max {
					return fmt.Errorf("done")
				}
			}
		}
		return nil
	})
	if n == 0 {
		return "no matches", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
