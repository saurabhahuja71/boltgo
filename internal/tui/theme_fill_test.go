package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// countUnpaintedSpaces reports spaces/cells that have no active background SGR.
// These are the cells that show the terminal default background (often black).
func countUnpaintedSpaces(line string) int {
	activeBG := false
	unpainted := 0
	for i := 0; i < len(line); {
		if line[i] == '\x1b' && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && !((line[j] >= 'A' && line[j] <= 'Z') || (line[j] >= 'a' && line[j] <= 'z')) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				params := line[i+2 : j]
				if params == "" || params == "0" {
					activeBG = false
				} else {
					for _, p := range strings.Split(params, ";") {
						switch p {
						case "0":
							activeBG = false
						case "48":
							activeBG = true
						case "49":
							activeBG = false
						}
					}
				}
			}
			if j < len(line) {
				i = j + 1
			} else {
				i++
			}
			continue
		}
		r := rune(line[i])
		size := 1
		if r >= utf8RuneSelf {
			// cheap: treat as single byte for space check; non-spaces won't match ' '
			if line[i] != ' ' && line[i] != '\t' {
				i++
				continue
			}
		}
		if (line[i] == ' ' || line[i] == '\t') && !activeBG {
			unpainted++
		}
		_ = size
		i++
	}
	return unpainted
}

const utf8RuneSelf = 0x80

func lightModel(t *testing.T, width, height int) model {
	t.Helper()
	previous := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := testModel(t)
	m.themeName = "light"
	applyTheme("light")
	applyTextareaTheme(&m.ta, "light")
	m.todosOpen = true
	m.width, m.height = width, height
	m.relayout()
	m.status = "ready"
	m.lines = []chatLine{
		{role: "user", text: "advice"},
		{role: "assistant", text: "normal assistant prose\n\n- item one\n- item two\n\n```go\nfmt.Println(1)\n```"},
		{role: "tool", text: "read_file · ok"},
		{role: "error", text: "example error"},
	}
	m.refreshViewport()
	return m
}

func TestLightThemeFinalViewPaintsAllocatedRowWidths(t *testing.T) {
	m := lightModel(t, 160, 40)
	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) < 10 {
		t.Fatalf("view too short: %d lines", len(lines))
	}

	var sawYou, sawAgent, sawStatus, sawWorkspace, sawInput, sawFooter, sawTodos bool
	for i, line := range lines {
		plain := ansiSeq.ReplaceAllString(line, "")
		w := lipgloss.Width(line)
		if w != 160 {
			// Allow the final line to be shorter only if empty EOF quirk; otherwise fail.
			if strings.TrimSpace(plain) != "" || i < len(lines)-1 {
				t.Fatalf("line %d width=%d want 160 plain=%q", i, w, truncateForTest(plain, 60))
			}
		}
		unpainted := countUnpaintedSpaces(line)
		if unpainted > 0 {
			t.Fatalf("line %d has %d unpainted spaces (terminal-default cells): plain=%q", i, unpainted, truncateForTest(plain, 60))
		}
		if strings.Contains(line, "48;2;15;23;42") || strings.Contains(line, "48;2;2;6;23") || strings.Contains(line, "48;2;0;0;0") {
			t.Fatalf("line %d has explicit dark/black background in light theme: plain=%q", i, truncateForTest(plain, 60))
		}
		switch {
		case strings.Contains(plain, "You"):
			sawYou = true
		case strings.Contains(plain, "Agent"):
			sawAgent = true
		case strings.Contains(plain, "Bolt | Permission"):
			sawStatus = true
		case strings.Contains(plain, "📁"):
			sawWorkspace = true
		case strings.Contains(plain, "❯") || strings.Contains(plain, "Message"):
			sawInput = true
		case strings.Contains(plain, "Ctrl+Q quit"):
			sawFooter = true
		case strings.Contains(plain, "Todos") || strings.Contains(plain, "No todos"):
			sawTodos = true
		}
	}
	for name, ok := range map[string]bool{
		"You": sawYou, "Agent": sawAgent, "status": sawStatus, "workspace": sawWorkspace,
		"input": sawInput, "footer": sawFooter, "todos": sawTodos,
	} {
		if !ok {
			t.Fatalf("missing %s row in light final view", name)
		}
	}
}

func TestLightThemeHasLightBackgroundNotUnpaintedCells(t *testing.T) {
	m := lightModel(t, 160, 40)
	view := m.View()
	if !strings.Contains(view, "48;2;255;255;255") && !strings.Contains(view, "48;2;248;250;252") && !strings.Contains(view, "48;5;255") {
		t.Fatalf("light view missing deliberate light backgrounds")
	}
	// No pure black background escape.
	if strings.Contains(view, "48;2;0;0;0") || strings.Contains(view, "48;5;0m") {
		t.Fatal("light view emitted explicit black background")
	}
	for i, line := range strings.Split(view, "\n") {
		if n := countUnpaintedSpaces(line); n > 0 {
			t.Fatalf("unpainted cells on line %d: %d", i, n)
		}
	}
}

func TestThemeToggleRepaintsFullWidthLight(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := testModel(t)
	m.width, m.height = 160, 40
	m.todosOpen = true
	m.relayout()
	m.lines = append(m.lines, chatLine{role: "user", text: "hello"}, chatLine{role: "assistant", text: "world"})
	m.refreshViewport()

	// dark -> black -> light
	for i := 0; i < 2; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = updated.(model)
	}
	if m.themeName != "light" {
		t.Fatalf("theme=%q", m.themeName)
	}
	light1 := m.View()

	// light -> dark -> black -> light (full cycle)
	for i := 0; i < 3; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = updated.(model)
	}
	if m.themeName != "light" {
		t.Fatalf("theme=%q after cycle", m.themeName)
	}
	light2 := m.View()

	for name, view := range map[string]string{"first light": light1, "cycled light": light2} {
		for i, line := range strings.Split(view, "\n") {
			if n := countUnpaintedSpaces(line); n > 0 {
				t.Fatalf("%s line %d unpainted=%d", name, i, n)
			}
			if strings.Contains(line, "48;2;0;0;0") {
				t.Fatalf("%s line %d has black bg", name, i)
			}
		}
	}
}

func TestDarkThemeStillPaintsFullWidth(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	applyTheme("dark")
	m := testModel(t)
	m.themeName = "dark"
	applyTextareaTheme(&m.ta, "dark")
	m.width, m.height = 160, 40
	m.todosOpen = true
	m.relayout()
	m.lines = []chatLine{{role: "user", text: "hi"}, {role: "assistant", text: "there"}}
	m.refreshViewport()
	view := m.View()
	if !strings.Contains(view, "48;2;15;23;42") {
		t.Fatal("dark theme missing slate background")
	}
	for i, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) != 160 && strings.TrimSpace(ansiSeq.ReplaceAllString(line, "")) != "" {
			t.Fatalf("dark line %d width=%d", i, lipgloss.Width(line))
		}
		if n := countUnpaintedSpaces(line); n > 0 {
			t.Fatalf("dark line %d unpainted=%d", i, n)
		}
	}
}

func TestInputLightThemeUsesLightFaces(t *testing.T) {
	m := lightModel(t, 120, 30)
	view := m.ta.View()
	if strings.Contains(view, "38;2;56;189;248") { // dark accent
		t.Fatalf("textarea retained dark accent: %q", view)
	}
	if strings.Contains(view, "38;2;226;232;240") { // dark foreground
		t.Fatalf("textarea retained dark foreground: %q", view)
	}
	if !strings.Contains(view, "48;2;255;255;255") {
		t.Fatalf("textarea missing light background: %q", view)
	}
	if got := strings.Count(view, "❯"); got != 1 {
		t.Fatalf("prompt markers=%d", got)
	}
}

func truncateForTest(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func TestPaintRowCoversWidth(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	applyTheme("light")
	row := paintRow(styleStatus, 80, "Bolt | Ready")
	if lipgloss.Width(row) != 80 {
		t.Fatalf("width=%d", lipgloss.Width(row))
	}
	if n := countUnpaintedSpaces(row); n > 0 {
		t.Fatalf("unpainted=%d in %q", n, row)
	}
	if !strings.Contains(row, "48;2;255;255;255") {
		t.Fatal("missing light bg")
	}
}

func assertLightFrame(t *testing.T, label, view string) {
	t.Helper()
	if strings.Contains(view, "48;2;0;0;0") || strings.Contains(view, "48;5;0m") {
		t.Fatalf("%s: explicit black background present", label)
	}
	if strings.Contains(view, "48;2;15;23;42") || strings.Contains(view, "48;2;2;6;23") {
		t.Fatalf("%s: stale dark component background present", label)
	}
	if !strings.Contains(view, "48;2;255;255;255") {
		t.Fatalf("%s: missing light canvas background", label)
	}
	for i, line := range strings.Split(view, "\n") {
		if n := countUnpaintedSpaces(line); n > 0 {
			t.Fatalf("%s: line %d has %d unpainted cells", label, i, n)
		}
	}
}

func TestPTYAcceptanceFreshLightAndThemeCycles(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := lightModel(t, 160, 40)
	assertLightFrame(t, "fresh light", m.View())

	m = testModel(t)
	m.width, m.height = 160, 40
	m.todosOpen = true
	m.relayout()
	m.lines = []chatLine{{role: "user", text: "hello"}, {role: "assistant", text: "world"}}
	m.refreshViewport()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB}) // black
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlB}) // light
	m = updated.(model)
	assertLightFrame(t, "dark→light", m.View())

	for i := 0; i < 3; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = updated.(model)
	}
	if m.themeName != "light" {
		t.Fatalf("expected light after full cycle, got %q", m.themeName)
	}
	assertLightFrame(t, "light→dark→light", m.View())
}
