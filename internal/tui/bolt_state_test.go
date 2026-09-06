package tui

import (
	"github.com/charmbracelet/bubbletea"
	"github.com/saurabhahuja71/agenterm/internal/agent"
	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
	"os"
	"path/filepath"
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
