package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve validates a user-supplied workspace. Workspace selection requires an
// absolute path so Bolt never guesses a sibling or scans a parent directory.
// Symlinked roots are rejected; callers can use the resolved real path instead.
func Resolve(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("workspace path must be absolute; use /workspace /absolute/path")
	}
	path := filepath.Clean(raw)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("workspace path %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %s is not a directory", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path %s: %w", path, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path %s: %w", path, err)
	}
	if real != path {
		return "", fmt.Errorf("workspace path %s is a symlink; use the resolved absolute path %s", path, real)
	}
	if f, err := os.Open(path); err != nil {
		return "", fmt.Errorf("workspace path %s is inaccessible: %w", path, err)
	} else {
		_ = f.Close()
	}
	return path, nil
}

// Outside reports whether candidate is outside active. Both paths are
// expected to have passed Resolve; the function also fails closed on errors.
func Outside(active, candidate string) bool {
	active, err := filepath.Abs(filepath.Clean(active))
	if err != nil {
		return true
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(active, candidate)
	return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}
