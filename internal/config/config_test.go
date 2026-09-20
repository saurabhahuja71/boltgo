package config

import (
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
