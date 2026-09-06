package config

import (
	"path/filepath"
	"testing"
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
