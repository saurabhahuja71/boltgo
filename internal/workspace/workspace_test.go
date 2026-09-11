package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRequiresAbsoluteExistingDirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(filepath.Base(root)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path error = %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file path error = %v", err)
	}
	if _, err := Resolve(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing workspace unexpectedly resolved")
	}
}

func TestResolveRejectsSymlinkedRoot(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestOutsideDoesNotScanOrGuess(t *testing.T) {
	root, sibling := t.TempDir(), t.TempDir()
	if !Outside(root, sibling) || Outside(root, filepath.Join(root, "child")) {
		t.Fatal("workspace containment classification incorrect")
	}
}
