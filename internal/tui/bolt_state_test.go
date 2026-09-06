package tui

import (
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/saurabhahuja71/agenterm/internal/agent"
	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testModel(t *testing.T) model {
	t.Helper()
	cfg := config.Default()
	cfg.PermissionMode = "ask"
	ag := agent.New(cfg, llm.New("http://127.0.0.1:1", "test"), tools.DefaultBuiltins(false))
	return New(Deps{Title: "Bolt", Summary: "test", Agent: ag})
}

func TestBoltShortcutsAndFooterState(t *testing.T) {
	m := testModel(t)
	for _, key := range []struct {
		key   tea.KeyType
		check func(model) bool
	}{
		{tea.KeyCtrlR, func(x model) bool { return x.permissionMode == permissions.ModeAllow }},
		{tea.KeyCtrlL, func(x model) bool { return x.mouseMode == "INTERACTIVE" }},
		{tea.KeyCtrlY, func(x model) bool { return x.visionEnabled }},
		{tea.KeyCtrlT, func(x model) bool { return x.todosOpen }},
		{tea.KeyCtrlO, func(x model) bool { return x.commandsOpen }},
		{tea.KeyCtrlB, func(x model) bool { return x.themeName == "black" }},
	} {
		updated, _ := m.Update(tea.KeyMsg{Type: key.key})
		m = updated.(model)
		if !key.check(m) {
			t.Fatalf("shortcut %v did not update state", key.key)
		}
	}
	view := m.View()
	for _, want := range []string{"Bolt | Permission Mode: ALLOW", "Mouse Mode: INTERACTIVE", "Vision: ON", "Tokens: —", "Ctrl+Q"} {
		if !containsText(view, want) {
			t.Fatalf("footer missing %q: %s", want, view)
		}
	}
}

func TestApprovalEventRendersChoicesAndRoutesDecision(t *testing.T) {
	m := testModel(t)
	response := make(chan permissions.Decision, 1)
	updated, _ := m.applyStreamEvent(agent.Event{Kind: agent.EventPermission, Permission: &permissions.Request{Tool: "run_shell", Level: permissions.LevelConfirm, Arguments: `{"command":"echo hi"}`}, Decision: response})
	if !containsText(updated.View(), "Approval required") || !containsText(updated.View(), "Allow permanently") {
		t.Fatal("approval UI missing")
	}
	result, _ := updated.handleApprovalKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	got := <-response
	if got != permissions.AllowSession || result.(*model).pendingApproval != nil {
		t.Fatalf("approval result=%q", got)
	}
}

func TestEventDoneKeepsInputBlockedUntilStreamCloses(t *testing.T) {
	m := testModel(t)
	m.busy = true
	updated, _ := m.applyStreamEvent(agent.Event{Kind: agent.EventDone})
	if !updated.busy || updated.status != "finalizing…" {
		t.Fatalf("EventDone reopened input: busy=%v status=%q", updated.busy, updated.status)
	}
	closed, _ := updated.Update(streamClosedMsg{})
	if closed.(model).busy {
		t.Fatal("stream close did not finish turn")
	}
}

func TestInputRemainsEditableAndQueuesRequestsFIFO(t *testing.T) {
	m := testModel(t)
	m.busy = true
	m.status = "streaming…"
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("next request")})
	m = updated.(model)
	if m.ta.Value() != "next request" {
		t.Fatalf("input was not editable while busy: %q", m.ta.Value())
	}
	m.ta.SetValue("second line\nwith indentation")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if len(m.pendingRequests) != 1 || m.pendingRequests[0] != "second line\nwith indentation" {
		t.Fatalf("queued request was not preserved exactly: %#v", m.pendingRequests)
	}
	if !strings.Contains(m.View(), "Queued · 1") {
		t.Fatal("queued request was not visible")
	}
	m.pendingRequests = append(m.pendingRequests, "third", "fourth")
	m.lines = append(m.lines, chatLine{role: "queued", text: "third"}, chatLine{role: "queued", text: "fourth"})
	if m.pendingRequests[0] != "second line\nwith indentation" || m.pendingRequests[2] != "fourth" {
		t.Fatalf("queue order changed: %#v", m.pendingRequests)
	}
}

func TestQueuedRequestStartsAfterActiveStreamCloses(t *testing.T) {
	m := testModel(t)
	m.busy = true
	m.lines = append(m.lines, chatLine{role: "queued", text: "follow-up"})
	m.pendingRequests = []string{"follow-up"}
	updated, _ := m.Update(streamClosedMsg{})
	m = updated.(model)
	defer func() {
		if m.cancel != nil {
			m.cancel()
		}
	}()
	if !m.busy || len(m.pendingRequests) != 0 {
		t.Fatalf("queued request did not become active: busy=%v queue=%#v", m.busy, m.pendingRequests)
	}
	if len(m.lines) < 2 || m.lines[len(m.lines)-1].role != "assistant-stream" {
		t.Fatalf("next request did not start an assistant turn: %#v", m.lines)
	}
}

func TestWindowSizeKeepsFixedRegionsUsable(t *testing.T) {
	m := testModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 36})
	m = updated.(model)
	if m.vp.Width != 98 || m.vp.Height < 5 || m.ta.Width() < 20 {
		t.Fatalf("resize dimensions: viewport=%dx%d input=%d", m.vp.Width, m.vp.Height, m.ta.Width())
	}
	view := m.View()
	for _, want := range []string{"Bolt |", "📁 ", "Message…"} {
		if !containsText(view, want) {
			t.Fatalf("resized view missing %q", want)
		}
	}
}

func TestTodoPanelUsesFixedRightSideAtNormalWidth(t *testing.T) {
	m := testModel(t)
	m.todosOpen = true
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	m = updated.(model)
	if !m.todoOnSide(120) || m.vp.Width != 85 {
		t.Fatalf("todo side layout not applied: side=%v viewport=%d", m.todoOnSide(120), m.vp.Width)
	}
	view := m.View()
	if strings.Index(view, "Todos") < 0 || strings.Index(view, "Todos") > strings.Index(view, "Bolt |") {
		t.Fatalf("todo panel was not rendered beside conversation before fixed footer")
	}
	if m.vp.Height < 20 {
		t.Fatalf("side todo panel incorrectly reduced conversation height: %d", m.vp.Height)
	}
}

func TestTodoPanelFallsBackAboveFooterWhenNarrow(t *testing.T) {
	m := testModel(t)
	m.todosOpen = true
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 36})
	m = updated.(model)
	if m.todoOnSide(80) || m.vp.Width != 78 {
		t.Fatalf("narrow todo layout did not use vertical fallback: side=%v viewport=%d", m.todoOnSide(80), m.vp.Width)
	}
	view := m.View()
	todoAt := strings.Index(view, "Todos")
	statusAt := strings.Index(view, "Bolt |")
	if todoAt < 0 || statusAt < 0 || todoAt > statusAt {
		t.Fatalf("narrow todo panel is not fixed above status/footer: todo=%d status=%d view=%q", todoAt, statusAt, view)
	}
}

func TestTodoToggleRelayoutsConversationWithoutChangingFixedFooter(t *testing.T) {
	m := testModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	m = updated.(model)
	before := m.vp.Width
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = updated.(model)
	if !m.todosOpen || m.vp.Width >= before {
		t.Fatalf("todo toggle did not reserve right panel: before=%d after=%d", before, m.vp.Width)
	}
	if !containsText(m.View(), "Bolt |") || !containsText(m.View(), "Message…") {
		t.Fatal("todo toggle displaced fixed footer/input")
	}
}

func TestTodoToggleAppearsInFinalView(t *testing.T) {
	m := testModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = updated.(model)
	view := m.View()
	if !m.todosOpen || !m.todoOnSide(160) {
		t.Fatal("Ctrl+T did not enable the side todo panel")
	}
	if !strings.Contains(view, "Todos") || !strings.Contains(view, "(no todos)") {
		t.Fatalf("final view omitted empty todo panel: %q", view)
	}
	if strings.Index(view, "Todos") > strings.Index(view, "Bolt |") {
		t.Fatal("todo panel was composed below the fixed footer")
	}
	if m.vp.Width <= 0 || m.conversationWidth(160) <= 0 {
		t.Fatal("todo layout produced a zero-width conversation")
	}
}

func TestThemeChangeRebuildsVisibleStylesWithoutResettingState(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := testModel(t)
	m.todosOpen = true
	m.lines = append(m.lines, chatLine{role: "user", text: "keep conversation"})
	m.refreshViewport()
	before := m.View()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = updated.(model)
	after := m.View()
	if m.themeName != "black" || before == after {
		t.Fatalf("theme action did not change rendered output: state=%q", m.themeName)
	}
	if !m.todosOpen || !containsText(after, "keep conversation") {
		t.Fatal("theme change reset todo or conversation state")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = updated.(model)
	if m.themeName != "light" || m.View() == after {
		t.Fatalf("theme did not switch to light with new rendering: state=%q", m.themeName)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	if updated.(model).themeName != "dark" {
		t.Fatal("theme did not cycle back to dark")
	}
}

func TestLightThemeUsesWhiteBackgroundAndDarkText(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := testModel(t)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = updated.(model) // black
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
	m = updated.(model) // light
	if m.themeName != "light" {
		t.Fatalf("theme=%q, want light", m.themeName)
	}
	view := m.View()
	if !strings.Contains(view, "\x1b[48;2;255;255;255m") {
		t.Fatalf("light theme did not render a white background: %q", view)
	}
	if !strings.Contains(view, "\x1b[38;2;15;23;42") {
		t.Fatalf("light theme did not render dark foreground text: %q", view)
	}
}

func TestLightThemeHasNoStaleDarkComponentBackgrounds(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := testModel(t)
	m.themeName = "light"
	applyTheme("light")
	applyTextareaTheme(&m.ta, "light")
	m.todosOpen = true
	m.width, m.height = 120, 40
	m.relayout()
	m.lines = append(m.lines,
		chatLine{role: "user", text: "light user"},
		chatLine{role: "assistant", text: "light assistant"},
		chatLine{role: "tool", text: "tool status"},
	)
	m.refreshViewport()
	view := m.View()
	if !strings.Contains(view, "48;2;255;255;255") {
		t.Fatalf("light view has no light canvas background: %q", view)
	}
	if strings.Contains(view, "48;2;2;6;23") || strings.Contains(view, "48;2;15;23;42") {
		t.Fatalf("light view retained a dark component background: %q", view)
	}
	if !strings.Contains(view, "Todos") || !strings.Contains(view, "message…") || !strings.Contains(view, "Bolt |") {
		t.Fatalf("light view lost a themed component: %q", view)
	}
}

func TestConversationBackgroundsStayOnHeaders(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	applyTheme("dark")

	m := testModel(t)
	m.vp.Width = 72
	m.lines = []chatLine{
		{role: "user", text: "normal user prose\nwith a second line"},
		{role: "assistant-stream", text: "normal assistant prose\nwith a second line"},
		{role: "thinking", text: "Thinking... 6s"},
	}
	m.refreshViewport()
	content := m.vp.View()
	backgrounds := strings.Count(content, "48;2;")
	if backgrounds != 3 {
		t.Fatalf("conversation painted %d background regions, want one per header: %q", backgrounds, content)
	}
}

func TestConversationMarkdownCodeStylingIsRetained(t *testing.T) {
	previous := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(previous)
	lipgloss.SetColorProfile(termenv.TrueColor)
	applyTheme("dark")

	m := testModel(t)
	m.vp.Width = 72
	m.lines = []chatLine{{role: "assistant", text: "```go\nfmt.Println(\"hello\")\n```"}}
	body := m.renderAssistantBody(&m.lines[0], m.vp.Width)
	logical := ansiForTest.ReplaceAllString(body, "")
	logical = strings.TrimSpace(logical)
	if !strings.Contains(logical, "fmt.Println(\"hello\")") {
		t.Fatalf("rendered code block omitted source: %q", logical)
	}
	if strings.Contains(logical, "│") {
		t.Fatalf("renderer decoration entered logical code content: %q", logical)
	}
	if !strings.Contains(body, "\x1b[") {
		t.Fatal("rendered code block lost Markdown styling")
	}
}

func TestFencedCodeRenderingPreservesLogicalLinesAndIndentation(t *testing.T) {
	m := testModel(t)
	m.lines = []chatLine{{role: "assistant", text: "```python\ndef one():\n    pass\n\ndef two():\n    pass\n```"}}
	logical := ansiForTest.ReplaceAllString(m.renderAssistantBody(&m.lines[0], 80), "")
	logical = strings.TrimSpace(logical)
	want := "def one():\n    pass\n\ndef two():\n    pass"
	if logical != want {
		t.Fatalf("fenced code structure changed:\n got %q\nwant %q", logical, want)
	}
	if strings.Contains(logical, "\n\n\n") || strings.Contains(logical, "│") {
		t.Fatalf("fenced code contains artificial spacing or decoration: %q", logical)
	}
}

var ansiForTest = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func TestInputUsesOnePromptMarker(t *testing.T) {
	m := testModel(t)
	m.width = 100
	m.height = 30
	m.relayout()
	empty := m.ta.View()
	if got := strings.Count(empty, "❯"); got != 1 {
		t.Fatalf("empty input rendered %d prompt markers, want 1: %q", got, empty)
	}
	m.ta.SetValue("single line")
	if got := strings.Count(m.ta.View(), "❯"); got != 1 {
		t.Fatalf("single-line input rendered %d prompt markers, want 1", got)
	}
	m.ta.SetValue("first\nsecond")
	if got := strings.Count(m.ta.View(), "❯"); got != 1 {
		t.Fatalf("multiline input rendered %d prompt markers, want 1", got)
	}
	m.ta.SetValue(strings.Repeat("wrapped input ", 30))
	if got := strings.Count(m.ta.View(), "❯"); got != 1 {
		t.Fatalf("wrapped input rendered %d prompt markers, want 1", got)
	}
	m.ta.Reset()
	if got := strings.Count(m.ta.View(), "❯"); got != 1 {
		t.Fatalf("reset input rendered %d prompt markers, want 1", got)
	}
}

func TestPlainArrowKeysScrollWhenInputIsEmpty(t *testing.T) {
	m := testModel(t)
	m.vp.Width = 80
	m.vp.Height = 5
	m.vp.SetContent(strings.Repeat("conversation line\n", 40))
	m.vp.GotoBottom()
	before := m.vp.YOffset
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if updated.(model).vp.YOffset >= before {
		t.Fatalf("plain up did not scroll: before=%d after=%d", before, updated.(model).vp.YOffset)
	}
	before = updated.(model).vp.YOffset
	updated, _ = updated.(model).Update(tea.KeyMsg{Type: tea.KeyDown})
	if updated.(model).vp.YOffset <= before {
		t.Fatalf("plain down did not scroll: before=%d after=%d", before, updated.(model).vp.YOffset)
	}
}

func TestHelpDocumentsBoltShortcuts(t *testing.T) {
	text := helpText()
	for _, want := range []string{"Ctrl+L          Toggle mouse mode", "Ctrl+Y          Toggle vision state", "Ctrl+T          Toggle todos", "Ctrl+O          Show commands", "Ctrl+B          Toggle theme", "Shift+Enter"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help missing %q", want)
		}
	}
}

func TestStreamingPreservesScrollIntent(t *testing.T) {
	m := testModel(t)
	m.vp.Width = 60
	m.vp.Height = 5
	m.lines = append(m.lines, chatLine{role: "assistant", text: strings.Repeat("long response ", 80)})
	m.refreshViewport()
	m.vp.GotoBottom()
	m.lines = append(m.lines, chatLine{role: "assistant", text: "newest"})
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Fatal("bottom-following stream did not stay at bottom")
	}
	m.vp.ScrollUp(4)
	before := m.vp.YOffset
	m.lines = append(m.lines, chatLine{role: "assistant-stream", text: strings.Repeat("more ", 40)})
	m.refreshViewport()
	if m.vp.YOffset != before {
		t.Fatalf("streaming yanked scrolled-up user to bottom: before=%d after=%d", before, m.vp.YOffset)
	}
}

func TestMouseWheelReachesConversationViewport(t *testing.T) {
	m := testModel(t)
	m.mouseMode = "INTERACTIVE"
	m.vp.Width = 80
	m.vp.Height = 5
	m.vp.SetContent(strings.Repeat("conversation line\n", 40))
	m.vp.GotoBottom()
	before := m.vp.YOffset
	updated, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if updated.(model).vp.YOffset >= before {
		t.Fatalf("wheel up did not move viewport: before=%d after=%d", before, updated.(model).vp.YOffset)
	}
}

func TestSlashSessionsUseWorkspaceStorage(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.deps.Workspace = workspace
	m.deps.SessionPath = filepath.Join(workspace, ".bolt", "sessions", "latest.json")
	m.deps.Agent.History = append(m.deps.Agent.History, llm.Message{Role: llm.RoleUser, Content: "workspace session"})
	updated, _ := m.handleSlash("/save named")
	m = updated.(model)
	path := filepath.Join(workspace, ".bolt", "sessions", "named.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if m.sessionPath("../outside") != "" {
		t.Fatal("session path escaped workspace")
	}
	m.deps.Agent.Reset()
	updated, _ = m.handleSlash("/load named")
	if len(updated.(model).deps.Agent.History) < 2 {
		t.Fatal("workspace session was not loaded")
	}
}

func containsText(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
