package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestEffectiveWorkspaceUsesBoltEnvironment(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("BOLT_WORKSPACE", workspace)
	got := (Config{}).Effective()
	want, _ := filepath.Abs(workspace)
	if got.Workspace != want {
		t.Fatalf("workspace = %q, want %q", got.Workspace, want)
	}
}

func TestLoadTracksExplicitPermissionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`permission_mode = "ask"
model = "configured-model"
base_url = "http://configured.example/v1"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTERM_CONFIG", path)
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PermissionModeConfigured || cfg.PermissionMode != "ask" {
		t.Fatalf("explicit permission mode was not retained: %#v", cfg)
	}
	if cfg.Model != "configured-model" || cfg.BaseURL != "http://configured.example/v1" {
		t.Fatalf("configured model/endpoint were not retained: %#v", cfg)
	}
}

func TestMCPStreamableHTTPConfigDecodes(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(`
[[mcp_servers]]
name = "zoho"
enabled = true
transport = "streamable_http"
url = "${ZOHO_MCP_URL}"
auth_env = "ZOHO_MCP_ACCESS_TOKEN"
`, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Transport != "streamable_http" || cfg.MCPServers[0].AuthEnv != "ZOHO_MCP_ACCESS_TOKEN" {
		t.Fatalf("decoded MCP config = %#v", cfg.MCPServers)
	}
}
