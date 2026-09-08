package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCollectWorktreeSummary(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "modified.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deleted.txt"), []byte("gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(dir, "modified.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), []byte{'x', 0, 'y'}, 0o644); err != nil {
		t.Fatal(err)
	}
	large := make([]byte, maxUntrackedBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), large, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := collectWorktreeSummary(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 5 || s.Additions != 3 || s.Deletions != 1 {
		t.Fatalf("summary=%+v, want 5 files +3 -1", s)
	}
	want := map[string]worktreeFile{
		"modified.txt": {Status: 'M', Additions: 1, Deletions: 0},
		"deleted.txt":  {Status: 'D', Additions: 0, Deletions: 1},
		"added.txt":    {Status: '?', Additions: 2, Deletions: 0},
		"binary.bin":   {Status: '?', Binary: true},
		"large.txt":    {Status: '?', Binary: true},
	}
	for _, f := range s.Files {
		got, ok := want[f.Path]
		if !ok || f.Status != got.Status || f.Additions != got.Additions || f.Deletions != got.Deletions || f.Binary != got.Binary {
			t.Fatalf("file %q = %+v, want %+v", f.Path, f, got)
		}
	}
}

func TestWorktreeRefreshIsAsyncAndRejectsStaleResults(t *testing.T) {
	m := model{deps: Deps{Workspace: t.TempDir()}, worktreeGeneration: 4}
	m.worktree.RefreshInProgress = true
	started := time.Now()
	updated, cmd := m.Update(worktreeDeltaMsg{Generation: 3, Summary: worktreeSummary{Files: []worktreeFile{{Path: "old"}}}})
	if cmd != nil || time.Since(started) > 100*time.Millisecond || len(updated.(model).worktree.Files) != 0 {
		t.Fatal("stale refresh changed state or blocked Update")
	}
	m = updated.(model)
	updated, _ = m.Update(worktreeDeltaMsg{Generation: 4, Summary: worktreeSummary{Files: []worktreeFile{{Path: "new"}}}})
	if got := updated.(model).worktree.Files[0].Path; got != "new" {
		t.Fatalf("fresh result path=%q", got)
	}
}

func TestCompactWorktreeSummaryAndView(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 120, 30
	m.relayout()
	m.worktree = worktreeSummary{Files: []worktreeFile{{Path: "app.go", Status: 'M'}, {Path: "new.go", Status: '?'}}, Additions: 42, Deletions: 17}
	got := compactWorktreeSummary(m.worktree, 120)
	if !strings.HasPrefix(got, "2 files · +42 -17") || !strings.Contains(got, "M app.go") {
		t.Fatalf("summary=%q", got)
	}
	view := m.View()
	if strings.Count(view, "2 files · +42 -17") != 1 {
		t.Fatalf("summary rendered more/less than once: %q", view)
	}
	if len(m.lines) != 0 {
		t.Fatal("worktree summary entered transcript")
	}
	if narrow := compactWorktreeSummary(m.worktree, 24); len(narrow) > 24 {
		t.Fatalf("narrow summary=%q", narrow)
	}
}
