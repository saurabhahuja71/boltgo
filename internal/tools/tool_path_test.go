package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirPathMatrix(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{".github/workflows", "nested_dir", "hyphen-dir", "underscore_dir"} {
		if err := os.MkdirAll(filepath.Join(workspace, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := DefaultBuiltinsOpts(BuiltinOpts{EnableShell: true, Workspace: workspace})
	tests := []struct {
		name string
		path string
		want string
	}{
		{"github directory", ".github", "workflows/"},
		{"github workflows", ".github/workflows", "ci.yml"},
		{"workspace dot", ".", ".github/"},
		{"absolute workspace", workspace, ".github/"},
		{"nested relative", "nested_dir", ""},
		{"underscore path", "underscore_dir", ""},
		{"hyphen path", "hyphen-dir", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pathJSON, err := json.Marshal(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			result := r.RunDetailed(context.Background(), "list_dir", `{"path":`+string(pathJSON)+`}`)
			if result.Category != FailureSuccess || !strings.Contains(result.Output, tt.want) {
				t.Fatalf("list_dir(%q) = %+v, want success containing %q", tt.path, result, tt.want)
			}
		})
	}

	missing := filepath.Join(workspace, "does-not-exist")
	pathJSON, _ := json.Marshal(missing)
	result := r.RunDetailed(context.Background(), "list_dir", `{"path":`+string(pathJSON)+`}`)
	if result.Category != FailureNotFound || !strings.Contains(strings.ToLower(result.Output), "no such file or directory") {
		t.Fatalf("missing list_dir = %+v, want not-found underlying error", result)
	}
}

func TestListDirRejectsMalformedArgumentsInsteadOfDefaultingToWorkspace(t *testing.T) {
	r := DefaultBuiltinsOpts(BuiltinOpts{Workspace: t.TempDir()})
	result := r.RunDetailed(context.Background(), "list_dir", "/tmp/not-json")
	if result.Category != FailureInvalidInput || !strings.Contains(result.Output, "invalid list_dir arguments") {
		t.Fatalf("malformed list_dir = %+v, want invalid-input diagnostic", result)
	}
}

func TestAllowModeDoesNotBypassWorkspaceScope(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := DefaultBuiltinsOpts(BuiltinOpts{EnableShell: true, Workspace: workspace})
	args, _ := json.Marshal(map[string]string{"path": secret})
	result := r.RunDetailed(context.Background(), "read_file", string(args))
	if result.Category != FailurePermissionDenied && !strings.Contains(result.Output, "outside the active workspace") {
		t.Fatalf("absolute read escaped scope: %+v", result)
	}
	result = r.RunDetailed(context.Background(), "list_dir", `{"path":"`+outside+`"}`)
	if !strings.Contains(result.Output, "outside the active workspace") {
		t.Fatalf("absolute list escaped scope: %+v", result)
	}
	result = r.RunDetailed(context.Background(), "run_shell", `{"command":"cat `+secret+`"}`)
	if !strings.Contains(result.Output, "outside the active workspace") {
		t.Fatalf("shell absolute path escaped scope: %+v", result)
	}
}
