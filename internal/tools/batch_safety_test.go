package tools

import (
	"context"
	"testing"
)

func TestReadOnlyBatchEligibleUsesConcreteBuiltins(t *testing.T) {
	r := DefaultBuiltins(false)
	for _, name := range []string{"read_file", "find_files", "grep", "list_dir", "repo_map"} {
		if !r.ReadOnlyBatchEligible(name) {
			t.Errorf("%s should be eligible for read-only batching", name)
		}
	}
	for _, name := range []string{"write_file", "str_replace", "run_shell", "run_tests", "git", "ssh_execute", "add_todo", "list_todos", "fetch"} {
		if r.ReadOnlyBatchEligible(name) {
			t.Errorf("%s must not be eligible for read-only batching", name)
		}
	}

	custom := NewRegistry()
	custom.Register(testBatchRunner{name: "read_file"})
	if custom.ReadOnlyBatchEligible("read_file") {
		t.Fatal("custom runner sharing a built-in name must remain sequential")
	}
}

type testBatchRunner struct{ name string }

func (r testBatchRunner) Name() string                                  { return r.name }
func (testBatchRunner) Description() string                             { return "test" }
func (testBatchRunner) Schema() map[string]any                          { return map[string]any{} }
func (testBatchRunner) Run(_ context.Context, _ string) (string, error) { return "", nil }
