package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoTestUsesPerWorkspaceCacheWhenInheritedCacheIsInvalid(t *testing.T) {
	workspace := writeGoFixture(t, false)
	badCache := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(badCache, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := runTests{Workspace: workspace}
	old := os.Getenv("GOCACHE")
	if err := os.Setenv("GOCACHE", badCache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("GOCACHE", old) })
	out, err := runner.Run(context.Background(), `{"command":"go test ./..."}`)
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("go test with invalid inherited cache: err=%v out=%q", err, out)
	}
	cache := filepath.Join(workspace, ".bolt", "go-cache")
	if !writableCacheDir(cache) {
		t.Fatalf("isolated cache was not created: %s", cache)
	}
}

func TestWritableUserGoCacheIsPreserved(t *testing.T) {
	workspace := writeGoFixture(t, false)
	cache := t.TempDir()
	env, err := testCommandEnvironmentWithEnv(context.Background(), workspace, "go test ./...", []string{"PATH=" + os.Getenv("PATH"), "GOCACHE=" + cache})
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupEnv(env, "GOCACHE"); got != cache {
		t.Fatalf("cache env helper changed valid cache: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".bolt")); !os.IsNotExist(err) {
		t.Fatalf("isolated cache created despite valid user cache: %v", err)
	}

	old := os.Getenv("GOCACHE")
	if err := os.Setenv("GOCACHE", cache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("GOCACHE", old) })
	if out, err := (runTests{Workspace: workspace}).Run(context.Background(), `{"command":"go test ./..."}`); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("go test with valid user cache: err=%v out=%q", err, out)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".bolt")); !os.IsNotExist(err) {
		t.Fatalf("isolated cache created during valid user-cache run: %v", err)
	}
}

func TestNonGoCommandReceivesNoGoCacheOverride(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH")}
	got, err := testCommandEnvironmentWithEnv(context.Background(), t.TempDir(), "printf '%s' hello", env)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(env) || lookupEnv(got, "GOCACHE") != "" {
		t.Fatalf("non-Go command environment changed: before=%v after=%v", env, got)
	}
}

func TestSeparateWorkspacesReceiveSeparateCaches(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	bad := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(bad, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnv := []string{"PATH=" + os.Getenv("PATH"), "GOCACHE=" + bad}
	firstEnv, err := testCommandEnvironmentWithEnv(context.Background(), first, "go test ./...", baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	secondEnv, err := testCommandEnvironmentWithEnv(context.Background(), second, "go test ./...", baseEnv)
	if err != nil {
		t.Fatal(err)
	}
	if lookupEnv(firstEnv, "GOCACHE") == lookupEnv(secondEnv, "GOCACHE") {
		t.Fatalf("workspace caches were shared: %q", lookupEnv(firstEnv, "GOCACHE"))
	}
}

func TestGoTestFailureRemainsCommandFailure(t *testing.T) {
	workspace := writeGoFixture(t, true)
	old := os.Getenv("GOCACHE")
	cache := t.TempDir()
	if err := os.Setenv("GOCACHE", cache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("GOCACHE", old) })
	out, err := (runTests{Workspace: workspace}).Run(context.Background(), `{"command":"go test ./..."}`)
	if err == nil {
		t.Fatalf("expected genuine test failure: %q", out)
	}
	if result := resultFromOutput(out, err); result.Category != FailureCommand {
		t.Fatalf("test failure category=%s, want %s", result.Category, FailureCommand)
	}
}

func TestGoCacheSetupFailureIsEnvironmentFailure(t *testing.T) {
	workspaceFile := filepath.Join(t.TempDir(), "workspace-file")
	if err := os.WriteFile(workspaceFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	badCache := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(badCache, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := testCommandEnvironmentWithEnv(context.Background(), workspaceFile, "go test ./...", []string{"PATH=" + os.Getenv("PATH"), "GOCACHE=" + badCache}); err == nil {
		t.Fatal("expected cache setup failure")
	} else if result := resultFromOutput("", err); result.Category != FailureEnvironment {
		t.Fatalf("setup failure category=%s, want %s", result.Category, FailureEnvironment)
	}
}

func writeGoFixture(t *testing.T, failing bool) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/cachefixture\n\ngo 1.20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testBody := `package cachefixture

import "testing"

func TestFixture(t *testing.T) {
`
	if failing {
		testBody += "\tt.Fatal(\"intentional test failure\")\n"
	}
	testBody += "}\n"
	if err := os.WriteFile(filepath.Join(workspace, "cache_test.go"), []byte(testBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace
}
