package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

func TestOperationalConfigPathsAreReadOnlyExceptions(t *testing.T) {
	if !isOperationalConfigPath("/home/sauahuja/.kube/config-sidb1flannel") {
		t.Fatal("sidb kubeconfig should be recognized as an operational config")
	}
	if !isOperationalConfigPath("/home/sauahuja/.ssh/config") {
		t.Fatal("SSH config should be recognized as an operational config")
	}
	if isOperationalConfigPath("/home/sauahuja/.ssh/id_rsa") {
		t.Fatal("private keys must not be operational config exceptions")
	}
	if !operationalConfigCommand("KUBECONFIG=/home/sauahuja/.kube/config-sidb1flannel kubectl get pods") {
		t.Fatal("kubectl command should be allowed to reference kubeconfig")
	}
	if operationalConfigCommand("rm -f /home/sauahuja/.kube/config-sidb1flannel") {
		t.Fatal("destructive command must not be allowed by operational exception")
	}
}

func TestNormalizeOperationalCommand(t *testing.T) {
	got := normalizeOperationalCommand("ssh -T bastion true")
	if !strings.Contains(got, "-F '") || !strings.Contains(got, "/.ssh/config'") {
		t.Fatalf("SSH command was not bound to explicit config: %q", got)
	}
	if got := normalizePortCheck("ss -ltn sport = :6449"); got == "ss -ltn sport = :6449" {
		if _, err := exec.LookPath("ss"); err != nil {
			t.Fatal("missing ss should be normalized to netstat")
		}
	}
}

func TestRemoteSSHCommandsUseSSHExecute(t *testing.T) {
	if got := shellCommandBlocked("ssh podman9 podman images"); !strings.Contains(got, "ssh_execute") {
		t.Fatalf("remote SSH command was not redirected: %q", got)
	}
	if got := shellCommandBlocked("ssh -L 6449:127.0.0.1:6443 podman9 -N"); got != "" {
		t.Fatalf("SSH tunnel was incorrectly blocked: %q", got)
	}
}

func TestSSHConfigInspectionUsesSSHExecute(t *testing.T) {
	if got := shellCommandBlocked("cat ~/.ssh/config | grep podman9"); !strings.Contains(got, "ssh_execute") {
		t.Fatalf("SSH config inspection was not redirected: %q", got)
	}
}

func TestSSHConfigReadIsDelegatedToSSHExecute(t *testing.T) {
	tool := readFile{Workspace: t.TempDir()}
	got, err := tool.Run(context.Background(), `{"path":"~/.ssh/config"}`)
	if err != nil || !strings.Contains(got, "ssh_execute") {
		t.Fatalf("SSH config read was not delegated: output=%q err=%v", got, err)
	}
}

func TestSSHTunnelPortDetection(t *testing.T) {
	for _, tt := range []struct {
		command string
		port    string
		ok      bool
	}{
		{"ssh -N -L 6449:10.0.2.65:6443 bastion", "6449", true},
		{"ssh -N -L 127.0.0.1:6449:10.0.2.65:6443 bastion", "6449", true},
		{"ssh podman9 podman images", "", false},
	} {
		port, ok := sshTunnelPort(tt.command)
		if port != tt.port || ok != tt.ok {
			t.Fatalf("sshTunnelPort(%q) = %q, %v; want %q, %v", tt.command, port, ok, tt.port, tt.ok)
		}
	}
}
