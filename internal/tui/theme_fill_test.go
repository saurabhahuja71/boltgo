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

func TestContentMaxWidthCapsReadableConversation(t *testing.T) {
	if got := contentMaxWidth(180); got != 130 {
		t.Fatalf("wide contentMaxWidth=%d want 130", got)
	}
	if got := contentMaxWidth(110); got != 110 {
		t.Fatalf("mid contentMaxWidth=%d want 110", got)
	}
	if got := contentMaxWidth(90); got != 90 {
		t.Fatalf("narrow-ish contentMaxWidth=%d want 90", got)
	}
	m := testModel(t)
	m.width, m.height = 180, 40
	m.relayout()
	if m.messageWidth() > 130 {
		t.Fatalf("messageWidth=%d exceeds readable cap", m.messageWidth())
	}
	if m.vp.Width != 180 {
		t.Fatalf("viewport should use remaining conversation width, got width=%d", m.vp.Width)
	}
}

func TestStatusLineIsCompactSubtle(t *testing.T) {
	m := lightModel(t, 160, 40)
	view := m.View()
	plain := ansiSeq.ReplaceAllString(view, "")
	if !strings.Contains(plain, "Bolt ·") {
		t.Fatalf("missing compact status: %q", plain)
	}
	if strings.Contains(plain, "Permission Mode:") || strings.Contains(plain, "Mouse Mode:") {
		t.Fatalf("status still uses heavy labels: %q", plain)
	}
	// The canvas is intentionally painted across the row. Verify the status
	// remains a single compact line rather than treating its canvas as a bar.
	statusLines := 0
	for _, line := range strings.Split(view, "\n") {
		if !strings.Contains(line, "· ASK ·") {
			continue
		}
		statusLines++
	}
	if statusLines != 1 {
		t.Fatalf("status occupies %d lines, want 1", statusLines)
	}
}

func TestViewHasNoInputBorderBox(t *testing.T) {
	m := lightModel(t, 120, 30)
	view := m.View()
	plain := ansiSeq.ReplaceAllString(view, "")
	lines := strings.Split(plain, "\n")
	for _, line := range lines {
		if strings.Contains(line, "›") || strings.Contains(strings.ToLower(line), "message") {
			if strings.Contains(line, "╭") || strings.Contains(line, "╰") || strings.Contains(line, "│ ›") {
				t.Fatalf("input still appears boxed: %q", line)
			}
		}
	}
	if got := strings.Count(view, "›"); got != 1 {
		t.Fatalf("prompt markers=%d", got)
	}
}

func TestLightThemeFinalViewIsContentFirst(t *testing.T) {
	m := lightModel(t, 160, 40)
	view := m.View()
	plain := ansiSeq.ReplaceAllString(view, "")
	for _, want := range []string{"You", "advice", "Agent", "Bolt ·", "📁", "Ctrl+Q", "Todos"} {
		if !strings.Contains(plain, want) && !strings.Contains(strings.ToLower(plain), strings.ToLower(want)) {
			// Message placeholder uses ellipsis variant.
			if want == "📁" && strings.Contains(plain, "📁") {
				continue
			}
			if !strings.Contains(plain, want) {
				t.Fatalf("missing %q in light view", want)
			}
		}
	}
	if strings.Contains(view, "48;2;0;0;0") || strings.Contains(view, "48;5;0m") {
		t.Fatal("light view emitted explicit black background")
	}
	if strings.Contains(view, "48;2;2;6;23") || strings.Contains(view, "48;2;15;23;42") {
		t.Fatal("light view retained dark component backgrounds")
	}
	// The complete frame is painted to the terminal width so the light canvas
	// cannot leak the terminal's black default through unused cells.
	if lines := strings.Split(view, "\n"); len(lines) != 40 {
		t.Fatalf("view has %d rows, want 40", len(lines))
	}
}

func TestThemeToggleKeepsContentFirstLight(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := testModel(t)
	m.width, m.height = 160, 40
	m.todosOpen = true
	m.relayout()
	m.lines = append(m.lines, chatLine{role: "user", text: "hello"}, chatLine{role: "assistant", text: "world"})
	m.refreshViewport()

	for i := 0; i < 2; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = updated.(model)
	}
	if m.themeName != "light" {
		t.Fatalf("theme=%q", m.themeName)
	}
	view := m.View()
	if strings.Contains(view, "48;2;0;0;0") || strings.Contains(view, "48;2;15;23;42") || strings.Contains(view, "48;2;2;6;23") {
		t.Fatalf("light after toggle has dark/black fills: %q", view)
	}
	if !strings.Contains(view, "Bolt ·") || !strings.Contains(view, "hello") {
		t.Fatalf("light after toggle lost content: %q", ansiSeq.ReplaceAllString(view, ""))
	}
}

func TestDarkThemeContentFirstStillReadable(t *testing.T) {
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
	plain := ansiSeq.ReplaceAllString(view, "")
	if !strings.Contains(plain, "You") || !strings.Contains(plain, "Agent") || !strings.Contains(plain, "Bolt ·") {
		t.Fatalf("dark view missing core chrome/content: %q", plain)
	}
	if strings.Contains(view, "48;2;0;0;0") {
		t.Fatal("dark view emitted pure black fills")
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
		t.Fatalf("textarea missing light background face: %q", view)
	}
	if got := strings.Count(view, "›"); got != 1 {
		t.Fatalf("prompt markers=%d", got)
	}
}

func TestLightThemePaintsFooterCanvas(t *testing.T) {
	m := lightModel(t, 160, 40)
	view := m.View()
	for _, line := range strings.Split(view, "\n") {
		if !strings.Contains(line, "Ctrl+Q quit") {
			continue
		}
		if !strings.Contains(line, "48;2;255;255;255") {
			t.Fatalf("light footer did not paint its canvas: %q", line)
		}
		return
	}
	t.Fatal("light footer was not rendered")
}

func TestMessageWidthUsesReadableCapNotFullViewport(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 200, 40
	m.todosOpen = false
	m.relayout()
	m.lines = []chatLine{{role: "user", text: strings.Repeat("word ", 80)}}
	m.refreshViewport()
	content := ansiSeq.ReplaceAllString(m.vp.View(), "")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" || line == "You" {
			continue
		}
		if lipgloss.Width(line) > 130 {
			t.Fatalf("user prose line wider than readable cap: w=%d %q", lipgloss.Width(line), line)
		}
	}
}
