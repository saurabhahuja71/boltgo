package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	maxUntrackedBytes = 1 << 20
	gitRefreshTimeout = 3 * time.Second
)

type worktreeFile struct {
	Path      string
	Status    byte
	Additions int
	Deletions int
	Binary    bool
}

type worktreeSummary struct {
	Files             []worktreeFile
	Additions         int
	Deletions         int
	LastUpdated       time.Time
	RefreshInProgress bool
}

type worktreeDeltaMsg struct {
	Generation uint64
	Summary    worktreeSummary
	Err        error
}

// collectWorktreeSummary reads HEAD versus the current working tree. It is
// deliberately independent of Bubble Tea so it can run in a tea.Cmd.
func collectWorktreeSummary(ctx context.Context, workspace string) (worktreeSummary, error) {
	var summary worktreeSummary
	status, err := gitOutput(ctx, workspace, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return summary, err
	}
	numstat, err := gitOutput(ctx, workspace, "diff", "--numstat", "HEAD", "--")
	if err != nil {
		return summary, err
	}
	statusFiles := parsePorcelain(status)
	numstats := parseNumstat(numstat)
	for _, file := range statusFiles {
		if n, ok := numstats[file.Path]; ok {
			file.Additions, file.Deletions, file.Binary = n.add, n.del, n.binary
		} else if file.Status == '?' {
			file.Additions, file.Deletions, file.Binary = countUntracked(workspace, file.Path)
		}
		summary.Additions += file.Additions
		summary.Deletions += file.Deletions
		summary.Files = append(summary.Files, file)
	}
	summary.LastUpdated = time.Now()
	return summary, nil
}

func gitOutput(parent context.Context, workspace string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, gitRefreshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return out, nil
}

func parsePorcelain(raw []byte) []worktreeFile {
	var files []worktreeFile
	parts := bytes.Split(raw, []byte{0})
	for i := 0; i < len(parts); i++ {
		if len(parts[i]) < 4 || parts[i][2] != ' ' {
			continue
		}
		status := statusCode(parts[i][0], parts[i][1])
		path := string(parts[i][3:])
		// With -z, rename/copy records have a second NUL-delimited path.
		// The first path is the destination in Git's porcelain format.
		if (status == 'R' || status == 'C') && i+1 < len(parts) && len(parts[i+1]) > 0 {
			i++
		}
		files = append(files, worktreeFile{Path: path, Status: status})
	}
	return files
}

func statusCode(x, y byte) byte {
	for _, c := range []byte{x, y} {
		switch c {
		case 'D':
			return 'D'
		case 'A':
			return 'A'
		case 'R':
			return 'R'
		case 'C':
			return 'C'
		case 'M':
			return 'M'
		case '?':
			return '?'
		}
	}
	return 'M'
}

type parsedNumstat struct {
	add, del int
	binary   bool
}

func parseNumstat(raw []byte) map[string]parsedNumstat {
	out := make(map[string]parsedNumstat)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		path := fields[2]
		if fields[0] == "-" || fields[1] == "-" {
			out[path] = parsedNumstat{binary: true}
			if rename := strings.LastIndex(path, " => "); rename >= 0 {
				out[path[rename+4:]] = parsedNumstat{binary: true}
			}
			continue
		}
		add, errA := strconv.Atoi(fields[0])
		del, errD := strconv.Atoi(fields[1])
		if errA == nil && errD == nil {
			out[path] = parsedNumstat{add: add, del: del}
			if rename := strings.LastIndex(path, " => "); rename >= 0 {
				out[path[rename+4:]] = parsedNumstat{add: add, del: del}
			}
		}
	}
	return out
}

func countUntracked(workspace, name string) (int, int, bool) {
	path := filepath.Join(workspace, filepath.FromSlash(name))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxUntrackedBytes {
		return 0, 0, true
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return 0, 0, true
	}
	if len(b) == 0 {
		return 0, 0, false
	}
	return bytes.Count(b, []byte{'\n'}) + btoi(b[len(b)-1] != '\n'), 0, false
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func worktreeRefreshCmd(workspace string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		summary, err := collectWorktreeSummary(context.Background(), workspace)
		return worktreeDeltaMsg{Generation: generation, Summary: summary, Err: err}
	}
}

func compactWorktreeSummary(s worktreeSummary, width int) string {
	if len(s.Files) == 0 {
		return ""
	}
	base := fmt.Sprintf("%d files · +%d -%d", len(s.Files), s.Additions, s.Deletions)
	if len(s.Files) == 1 {
		base = fmt.Sprintf("1 file · +%d -%d", s.Additions, s.Deletions)
	}
	if width <= 0 || lipgloss.Width(base) >= width {
		if width > 0 {
			return truncateCells(base, width)
		}
		return base
	}
	for _, f := range s.Files {
		candidate := base + " · " + string(f.Status) + " " + filepath.ToSlash(f.Path)
		if lipgloss.Width(candidate) > width {
			break
		}
		base = candidate
	}
	return base
}
