package tools

import (
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

// runTests runs a project check command (default: go test ./... when go.mod exists).
type runTests struct {
	DefaultCmd string
	Workspace  string
}

func (runTests) Name() string { return "run_tests" }
func (r runTests) Description() string {
	def := r.DefaultCmd
	if def == "" {
		def = "auto (go test ./... if go.mod, else make test)"
	}
	return "Run project tests/checks. Default command: " + def + ". Use after edits to verify."
}
func (runTests) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Override check command (bash -lc). Empty = configured/auto default.",
			},
		},
	}
}

func (r runTests) Run(ctx context.Context, argsJSON string) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &in)
	cmdStr := strings.TrimSpace(in.Command)
	if cmdStr == "" {
		cmdStr = strings.TrimSpace(r.DefaultCmd)
	}
	if cmdStr == "" {
		cmdStr = autoTestCommand(r.Workspace)
	}
	if cmdStr == "" {
		return "", fmt.Errorf("no test command configured (set test_command in config or pass command)")
	}
	env, err := testCommandEnvironment(ctx, r.Workspace, cmdStr)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdStr)
	cmd.Dir = r.Workspace
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	s := string(out)
	if len(s) > 40_000 {
		s = s[:40_000] + "\n…[truncated]…"
	}
	if err != nil {
		return fmt.Sprintf("$ %s\n%s\n[exit error: %v]", cmdStr, s, err), err
	}
	return fmt.Sprintf("$ %s\n%s\nok", cmdStr, s), nil
}

var goTestCommandPattern = regexp.MustCompile(`(^|[;&|()[:space:]])go[[:space:]]+test([[:space:]]|$)`)

// environmentFailure marks setup failures separately from failures produced by
// the user's test command. The agent can report these as execution-environment
// failures without treating them as model or test failures.
type environmentFailure struct{ err error }

func (e environmentFailure) Error() string       { return e.err.Error() }
func (e environmentFailure) Unwrap() error       { return e.err }
func (e environmentFailure) EnvironmentFailure() {}

func testCommandEnvironment(ctx context.Context, workspace, command string) ([]string, error) {
	return testCommandEnvironmentWithEnv(ctx, workspace, command, os.Environ())
}

func testCommandEnvironmentWithEnv(ctx context.Context, workspace, command string, env []string) ([]string, error) {
	if !goTestCommandPattern.MatchString(command) {
		return env, nil
	}

	cachePath := lookupEnv(env, "GOCACHE")
	if cachePath == "" {
		cachePath = goDefaultCachePath(ctx, env)
	}
	if cachePath != "" && writableCacheDir(cachePath) {
		return env, nil
	}

	if strings.TrimSpace(workspace) == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, environmentFailure{err: fmt.Errorf("resolve test workspace for isolated Go cache: %w", err)}
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, environmentFailure{err: fmt.Errorf("resolve test workspace for isolated Go cache: %w", err)}
	}
	isolated := filepath.Join(workspace, ".bolt", "go-cache")
	if err := os.MkdirAll(isolated, 0o700); err != nil {
		return nil, environmentFailure{err: fmt.Errorf("create isolated Go cache: %w", err)}
	}
	if err := os.Chmod(isolated, 0o700); err != nil {
		return nil, environmentFailure{err: fmt.Errorf("secure isolated Go cache: %w", err)}
	}
	if !writableCacheDir(isolated) {
		return nil, environmentFailure{err: fmt.Errorf("isolated Go cache is not writable: %s", isolated)}
	}
	return setEnv(env, "GOCACHE", isolated), nil
}

func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(entry, prefix))
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	set := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !set {
				out = append(out, prefix+value)
				set = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !set {
		out = append(out, prefix+value)
	}
	return out
}

func goDefaultCachePath(ctx context.Context, env []string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "go", "env", "GOCACHE")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil || probeCtx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func writableCacheDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return cacheDirWritable(path)
}

func autoTestCommand(workspace string) string {
	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err == nil {
		return "go test ./..."
	}
	if _, err := os.Stat(filepath.Join(workspace, "Makefile")); err == nil {
		return "make test"
	}
	if _, err := os.Stat(filepath.Join(workspace, "package.json")); err == nil {
		return "npm test --if-present"
	}
	return ""
}
