package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWalkGrepHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := runWalkGrep(ctx, "needle", root, "", false, 40); !errors.Is(err, context.Canceled) {
		t.Fatalf("runWalkGrep error = %v, want context.Canceled", err)
	}
}
