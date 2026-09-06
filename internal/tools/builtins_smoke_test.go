package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsBasic(t *testing.T) {
	// Run from module root so relative paths match real agenterm usage.
	root := findModuleRoot(t)
	cwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	r := DefaultBuiltins(false)
	out, err := r.Run(context.Background(), "list_dir", `{"path":"."}`)
	if err != nil || !strings.Contains(out, "go.mod") {
		t.Fatalf("list_dir: %v %q", err, out)
	}
	out, err = r.Run(context.Background(), "read_file", `{"path":"README.md"}`)
	if err != nil || !strings.Contains(out, "agenterm") {
		t.Fatalf("read_file: %v %q", err, trunc(out, 80))
	}
	out, err = r.Run(context.Background(), "find_files", `{"name":"go.mod","root":"."}`)
	if err != nil || !strings.Contains(out, "go.mod") {
		t.Fatalf("find_files: %v %q", err, out)
	}
	// Invented "repo/" prefix should still resolve when README.md exists at cwd.
	out, err = r.Run(context.Background(), "read_file", `{"path":"repo/README.md"}`)
	if err != nil || !strings.Contains(out, "agenterm") {
		t.Fatalf("resolve repo/README: %v %q", err, trunc(out, 80))
	}
	// fetch is always registered
	out, err = r.Run(context.Background(), "fetch", `{"url":"https://example.com","timeout_sec":15}`)
	if err != nil || !strings.Contains(out, "HTTP") {
		t.Fatalf("fetch: %v %q", err, trunc(out, 120))
	}
	// shell optional registry
	rs := DefaultBuiltins(true)
	out, err = rs.Run(context.Background(), "run_shell", `{"command":"echo shell-ok && true"}`)
	if err != nil || !strings.Contains(out, "shell-ok") {
		t.Fatalf("run_shell: %v %q", err, out)
	}
	// Mass link crawl must be blocked (was freezing the TUI).
	out, err = rs.Run(context.Background(), "run_shell", `{"command":"wget -qO- https://example.com | grep href | xargs -n1 curl -I"}`)
	if err != nil || !strings.Contains(out, "blocked") {
		t.Fatalf("expected blocked crawl, got %v %q", err, out)
	}
}

func TestWorkspaceOptionControlsShellAndFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := DefaultBuiltinsOpts(BuiltinOpts{EnableShell: true, Workspace: workspace})
	out, err := r.Run(context.Background(), "read_file", `{"path":"note.txt"}`)
	if err != nil || !strings.HasSuffix(out, "\nworkspace") {
		t.Fatalf("read_file = %q, %v", out, err)
	}
	out, err = r.Run(context.Background(), "run_shell", `{"command":"pwd"}`)
	if err != nil || strings.TrimSpace(out) != workspace {
		t.Fatalf("run_shell pwd = %q, %v", out, err)
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
