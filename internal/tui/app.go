package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/saurabhahuja71/agenterm/internal/agent"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/todos"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// OSC / color-query replies that leak into stdin when libraries probe the TTY
// (classic first-launch garbage: "]11;rgb:fafa/fafa/fdfd\").
var reOSCLeak = regexp.MustCompile(
	`(?:\x1b)?\]\d+;[^\x07\x1b\\]*(?:\x07|\x1b\\)?` +
		`|\]\d+;rgb:[0-9a-fA-F/\\]+` +
		`|rgb:[0-9a-fA-F]{2,4}/[0-9a-fA-F]{2,4}/[0-9a-fA-F]{2,4}\\?`,
)

var (
	colorMuted  = lipgloss.Color("#94a3b8")
	colorAccent = lipgloss.Color("#38bdf8")
	colorUser   = lipgloss.Color("#a78bfa")
	colorAsst   = lipgloss.Color("#4ade80")
	colorTool   = lipgloss.Color("#fbbf24")
	colorError  = lipgloss.Color("#f87171")
	colorBorder = lipgloss.Color("#1e293b")

	// Bubble backgrounds: light grey (You) vs white/slate (Agent) so Q/A
	// are distinct on light terminals; AdaptiveColor keeps dark terminals readable.
	bgUser = lipgloss.AdaptiveColor{Light: "#e5e7eb", Dark: "#1e293b"} // light grey / slate
	bgAsst = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0f172a"} // white / near-black
	fgBody = lipgloss.AdaptiveColor{Light: "#1e293b", Dark: "#e2e8f0"}

	styleHeader = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Padding(0, 1)
	styleStatus = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 1)
	styleHelp   = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 1)
	styleUser   = lipgloss.NewStyle().Foreground(colorUser).Bold(true)
	styleAsst   = lipgloss.NewStyle().Foreground(colorAsst).Bold(true)
	styleTool   = lipgloss.NewStyle().Foreground(colorTool)
	styleErr    = lipgloss.NewStyle().Foreground(colorError)
	styleBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(0, 1)

	styleUserBubble = lipgloss.NewStyle().
			Background(bgUser).
			Foreground(fgBody).
			Padding(0, 1)
	styleAsstBubble = lipgloss.NewStyle().
			Background(bgAsst).
			Foreground(fgBody).
			Padding(0, 1)
)

// Deps wired from main.
type Deps struct {
	Title       string
	Summary     string
	Agent       *agent.Agent
	Workspace   string
	SessionPath string
	// SaveSession enables automatic checkpointing after a completed turn. A
	// failed explicit resume disables this so fresh history cannot overwrite a
	// missing or corrupt requested session.
	SaveSession bool
}

type chatLine struct {
	role string
	text string
	// mdCache holds glamour output for role=="assistant". Invalidated when text changes.
	mdCache string
	mdSrc   string // text that mdCache was built from
}

type model struct {
	deps   Deps
	vp     viewport.Model
	ta     textarea.Model
	lines  []chatLine
	width  int
	height int
	busy   bool
	status string
	// stream accumulates assistant tokens. Must be a pointer: Bubble Tea
	// copies model by value; a non-empty strings.Builder must not be copied.
	stream *strings.Builder
	// turnAssistant identifies the single assistant bubble for the active turn.
	// Thinking, streaming, and the final answer all update this line in place;
	// tool/error entries may be inserted around it without creating another Agent bubble.
	turnAssistant int
	renderer      *glamour.TermRenderer
	cancel        context.CancelFunc
	events        <-chan agent.Event
	busySince     time.Time
	gotToken      bool
	waitSecs      int
	// verbose shows full tool I/O and model preambles; default is quiet/compact.
	verbose bool
	// scrubLeft: remaining startup OSC scrub passes (color-query junk).
	scrubLeft int
	// modelPick: interactive /model list (Tab cycle, Enter select, Esc cancel).
	modelPick *modelPicker
	// paintThrottle: avoid full viewport rebuilds on every token (large answers hang).
	lastPaint        time.Time
	paintPending     bool
	permissionMode   permissions.Mode
	mouseMode        string
	visionEnabled    bool
	visionSupported  bool
	todosOpen        bool
	commandsOpen     bool
	themeName        string
	pendingApproval  *permissions.Request
	approvalDecision chan permissions.Decision
	tokenUsage       *llm.Usage
}

// paintInterval is the minimum time between streaming viewport rebuilds.
const paintInterval = 80 * time.Millisecond

// glamourMaxBytes skips markdown rendering above this size (too slow for TUI).
const glamourMaxBytes = 24_000

// bubbleMaxBytes uses cheap plain paint instead of lipgloss Width for large bodies.
const bubbleMaxBytes = 8_000

// modelPicker is the interactive model selector opened by /model.
type modelPicker struct {
	ids []string
	idx int // highlighted index
}

type streamEvMsg agent.Event
type streamClosedMsg struct{}
type busyTickMsg time.Time
type paintDueMsg struct{}

func New(deps Deps) model {
	ta := textarea.New()
	ta.Placeholder = "Message…  /help  /model  /quiet  /verbose"
	ta.Focus()
	ta.Prompt = "❯ "
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	// Enter sends. Shift+Enter and Alt+Enter insert a newline.
	ta.KeyMap.InsertNewline.SetEnabled(true)
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "alt+enter")

	vp := viewport.New(80, 20)
	// Pager keys that don't fight the focused textarea (no j/k/f/b/space).
	// Mouse wheel works when tea.WithMouseCellMotion is set in Run().
	vp.MouseWheelEnabled = true
	vp.MouseWheelDelta = 3
	vp.KeyMap = viewport.KeyMap{
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdn", "page down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "page up"),
		),
		HalfPageUp: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "½ page up"),
		),
		HalfPageDown: key.NewBinding(
			key.WithKeys("ctrl+d"),
			key.WithHelp("ctrl+d", "½ page down"),
		),
		Up: key.NewBinding(
			key.WithKeys("ctrl+up"),
			key.WithHelp("ctrl+↑", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("ctrl+down"),
			key.WithHelp("ctrl+↓", "down"),
		),
		Left:  key.NewBinding(key.WithKeys("ctrl+left"), key.WithHelp("ctrl+←", "left")),
		Right: key.NewBinding(key.WithKeys("ctrl+right"), key.WithHelp("ctrl+→", "right")),
	}

	// Fixed dark style — WithAutoStyle() queries OSC 11 (bg color) and the
	// reply often appears as garbage in the input line on first launch.
	r := newGlamourRenderer(80)

	m := model{
		deps:            deps,
		vp:              vp,
		ta:              ta,
		status:          "ready",
		stream:          &strings.Builder{},
		turnAssistant:   -1,
		renderer:        r,
		verbose:         false, // compact tools by default
		permissionMode:  deps.Agent.Permissions.Mode,
		mouseMode:       "SELECT",
		visionEnabled:   deps.Agent.Cfg.VisionEnabled,
		visionSupported: false,
		themeName:       "dark",
		scrubLeft:       5, // a few startup passes to catch late OSC replies
		lines: []chatLine{
			// One short banner — path lives above the prompt; tools stay in the status bar.
			{role: "system", text: deps.Summary + " · /help"},
		},
	}
	m.refreshViewport()
	return m
}

func (m *model) ensureStream() *strings.Builder {
	if m.stream == nil {
		m.stream = &strings.Builder{}
	}
	return m.stream
}

func (m model) Init() tea.Cmd {
	// Scrub any OSC color-query junk already sitting in the input buffer.
	return tea.Batch(textarea.Blink, scrubInputCmd(), tea.DisableMouse)
}

type scrubInputMsg struct{}

func scrubInputCmd() tea.Cmd {
	return func() tea.Msg { return scrubInputMsg{} }
}

func newGlamourRenderer(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 80
	}
	// Prefer explicit dark theme over AutoStyle (no TTY color probes).
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		r, _ = glamour.NewTermRenderer(glamour.WithWordWrap(width))
	}
	return r
}

func looksLikeTerminalGarbage(s string) bool {
	return strings.Contains(s, "]11;") ||
		strings.Contains(s, "]10;") ||
		strings.Contains(s, "rgb:") ||
		strings.Contains(s, "\x1b]") ||
		strings.Contains(s, "\x1b\\") ||
		strings.Contains(s, "\x07")
}

// scrubTerminalGarbage removes leaked OSC / rgb color-query fragments.
func scrubTerminalGarbage(s string) string {
	if s == "" {
		return s
	}
	out := reOSCLeak.ReplaceAllString(s, "")
	// Common partials if ESC was consumed already
	out = strings.ReplaceAll(out, "]11;", "")
	out = strings.ReplaceAll(out, "]10;", "")
	out = strings.ReplaceAll(out, "\x07", "")
	// ST is often a bare trailing backslash after rgb:... was stripped
	out = strings.Trim(out, " \t\r\n\\")
	// If almost nothing left but punctuation from probes, drop it
	if out != "" && !hasLetterOrDigit(out) && (strings.Contains(s, "rgb:") || strings.Contains(s, "]11") || strings.Contains(s, "]10")) {
		return ""
	}
	return strings.TrimSpace(out)
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

func busyTick() tea.Cmd {
	// Fast tick so spinner / Thinking… blink while waiting on Ollama or tools.
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return busyTickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case scrubInputMsg:
		if v := m.ta.Value(); v != "" {
			if cleaned := scrubTerminalGarbage(v); cleaned != v {
				m.ta.SetValue(cleaned)
				m.ta.CursorEnd()
			}
		}
		// Limited startup retries — OSC replies can arrive a few frames late.
		if m.scrubLeft > 0 {
			m.scrubLeft--
			return m, tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
				return scrubInputMsg{}
			})
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerH, helpH, cwdH, busyH := 1, 1, 1, 0
		inputH := m.ta.Height() + 2
		fixedExtra := 1
		if m.todosOpen {
			fixedExtra += len(todos.Global.Items()) + 3
		}
		if m.commandsOpen {
			fixedExtra += 10
		}
		if m.pendingApproval != nil {
			fixedExtra += 7
		}
		vpH := msg.Height - headerH - helpH - cwdH - busyH - inputH - fixedExtra
		if vpH < 5 {
			vpH = 5
		}
		m.vp.Width = max(20, msg.Width-2)
		m.vp.Height = vpH
		m.ta.SetWidth(max(20, msg.Width-4))
		m.renderer = newGlamourRenderer(max(40, msg.Width-8))
		m.refreshViewport()
		// Resize often coincides with first paint; scrub leaked OSC once more.
		if v := m.ta.Value(); v != "" {
			if cleaned := scrubTerminalGarbage(v); cleaned != v {
				m.ta.SetValue(cleaned)
				m.ta.CursorEnd()
			}
		}

	case tea.KeyMsg:
		// Interactive model picker takes priority over normal input.
		if m.modelPick != nil {
			return m.handleModelPickKeys(msg)
		}
		if m.pendingApproval != nil {
			return m.handleApprovalKey(msg)
		}
		// Chat scroll keys — handle before the textarea so large answers are reachable.
		if m.handleChatScrollKey(msg) {
			return m, nil
		}
		switch msg.String() {
		case "esc":
			// Cancel in-flight generation without quitting the TUI.
			if m.busy && m.cancel != nil {
				m.cancel()
				m.status = "cancelling… (Esc)"
				m.refreshViewport()
				return m, nil
			}
		case "ctrl+q":
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "ctrl+c":
			if m.busy && m.cancel != nil {
				// First Ctrl+C cancels generation; second (when idle) quits.
				m.cancel()
				m.status = "cancelling… (Ctrl+C again to quit)"
				m.refreshViewport()
				return m, nil
			}
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		case "ctrl+r":
			return m, m.cyclePermissionMode()
		case "ctrl+l":
			return m, m.toggleMouseMode()
		case "ctrl+y":
			m.visionEnabled = !m.visionEnabled
			if m.visionEnabled && !m.visionSupported {
				m.status = "vision requested (provider has no image capability)"
			} else if m.visionEnabled {
				m.status = "vision on"
			} else {
				m.status = "ready"
			}
			return m, nil
		case "ctrl+t":
			m.todosOpen = !m.todosOpen
			return m, nil
		case "ctrl+o":
			m.commandsOpen = !m.commandsOpen
			return m, nil
		case "ctrl+b":
			if m.themeName == "dark" {
				m.themeName = "black"
			} else {
				m.themeName = "dark"
			}
			applyTheme(m.themeName)
			m.status = "theme: " + m.themeName
			return m, nil
		case "enter":
			if m.busy {
				return m, nil
			}
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			return m.handleSubmit(text)
		}
		// While busy, arrow keys also scroll the transcript (textarea is inactive).
		if m.busy {
			switch msg.String() {
			case "up", "k":
				m.vp.ScrollUp(1)
				return m, nil
			case "down", "j":
				m.vp.ScrollDown(1)
				return m, nil
			}
		}

	case busyTickMsg:
		if m.busy {
			m.waitSecs = int(time.Since(m.busySince).Seconds())
			// Always animate while busy — quiet mode may hide tokens, so users still see activity.
			if !m.gotToken {
				m.status = fmt.Sprintf("Thinking… %ds · %s (Esc cancel)", m.waitSecs, m.deps.Agent.Cfg.Model)
			} else if m.status == "" || m.status == "ready" || strings.HasPrefix(m.status, "Thinking") {
				m.status = fmt.Sprintf("Thinking… %ds · working", m.waitSecs)
			}
			// Only rebuild chat when the Thinking placeholder is visible. Never re-paint
			// a large streaming answer every 200ms (that freezes the TUI).
			if m.showingThinkingOnly() {
				m.upsertThinkingPlaceholder()
				m.refreshViewport()
			}
			// Header spinner/status re-render via View() without viewport rebuild.
			return m, busyTick()
		}

	case paintDueMsg:
		if m.paintPending {
			m.paintPending = false
			m.refreshViewport()
		}

	case streamBatchMsg:
		// Coalesced tokens + the next control event from waitNext.
		var cmd tea.Cmd
		m, cmd = m.applyStreamEvent(agent.Event{Kind: agent.EventToken, Text: msg.tokenText})
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		var cmd2 tea.Cmd
		m, cmd2 = m.applyStreamEvent(msg.next)
		if cmd2 != nil {
			cmds = append(cmds, cmd2)
		}
		if m.events != nil {
			cmds = append(cmds, waitNext(m.events))
		}

	case streamEvMsg:
		ev := agent.Event(msg)
		var cmd tea.Cmd
		m, cmd = m.applyStreamEvent(ev)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.events != nil {
			cmds = append(cmds, waitNext(m.events))
		}

	case streamClosedMsg:
		m.clearThinkingPlaceholder()
		if m.verbose {
			m.flushStreamAsLine()
		} else {
			m.flushStreamAsLineQuiet()
		}
		m.busy = false
		m.gotToken = false
		m.waitSecs = 0
		m.paintPending = false
		m.status = "ready"
		m.events = nil
		m.cancel = nil
		// Auto-save rolling session after each turn (best-effort).
		if m.deps.SaveSession && m.deps.SessionPath != "" {
			_, _ = m.deps.Agent.SaveSessionPath(m.deps.SessionPath)
		}
		m.refreshViewport()
	}

	if !m.busy && m.modelPick == nil {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		cmds = append(cmds, cmd)
		// Drop OSC / color-query junk if it landed as "typed" characters.
		if v := m.ta.Value(); v != "" {
			if looksLikeTerminalGarbage(v) {
				if cleaned := scrubTerminalGarbage(v); cleaned != v {
					m.ta.SetValue(cleaned)
					m.ta.CursorEnd()
				}
			}
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m model) handleSubmit(text string) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(text, "/") {
		return m.handleSlash(text)
	}
	m.lines = append(m.lines, chatLine{role: "user", text: text})
	m.ensureStream().Reset()
	m.turnAssistant = -1
	m.busy = true
	m.gotToken = false
	m.waitSecs = 0
	m.busySince = time.Now()
	m.status = fmt.Sprintf("Thinking… (%s)", m.deps.Agent.Cfg.Model)
	// New turn: jump to the latest content so the question is visible.
	m.vp.GotoBottom()
	m.upsertThinkingPlaceholder()
	m.refreshViewport()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	// Large buffer + coalescing in waitNext; do not drop tokens (incomplete answers).
	ch := make(chan agent.Event, 8192)
	m.events = ch
	m.paintPending = false
	ag := m.deps.Agent

	go func() {
		_ = ag.RunUserMessage(ctx, text, func(ev agent.Event) {
			// Prefer non-blocking send; if full, block with cancel so we never drop
			// tokens (dropped tokens used to produce incomplete large answers).
			select {
			case ch <- ev:
			case <-ctx.Done():
			default:
				select {
				case ch <- ev:
				case <-ctx.Done():
				}
			}
		})
		close(ch)
	}()

	return m, tea.Batch(waitNext(ch), busyTick())
}

// waitNext reads the next event and coalesces consecutive tokens so the UI
// does not process thousands of single-character messages for large answers.
func waitNext(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		if ev.Kind != agent.EventToken {
			return streamEvMsg(ev)
		}
		// Merge as many pending tokens as are already buffered.
		var b strings.Builder
		b.WriteString(ev.Text)
		for {
			select {
			case next, ok := <-ch:
				if !ok {
					// Tokens then closed — deliver tokens; closed handled on next wait.
					return streamEvMsg(agent.Event{Kind: agent.EventToken, Text: b.String()})
				}
				if next.Kind == agent.EventToken {
					b.WriteString(next.Text)
					continue
				}
				return streamBatchMsg{tokenText: b.String(), next: next}
			default:
				return streamEvMsg(agent.Event{Kind: agent.EventToken, Text: b.String()})
			}
		}
	}
}

// streamBatchMsg is a coalesced token chunk plus the following non-token event.
type streamBatchMsg struct {
	tokenText string
	next      agent.Event
}

// showingThinkingOnly is true when chat shows the Thinking placeholder (no stream yet).
func (m *model) showingThinkingOnly() bool {
	if len(m.lines) == 0 {
		return true
	}
	last := m.lines[len(m.lines)-1]
	return last.role == "thinking" || (last.role == "assistant-stream" &&
		(strings.TrimSpace(last.text) == "" || strings.HasPrefix(strings.TrimSpace(last.text), "Thinking")))
}

// schedulePaint throttles expensive viewport rebuilds during streaming.
func (m *model) schedulePaint(force bool) tea.Cmd {
	if force {
		m.paintPending = false
		m.refreshViewport()
		return nil
	}
	now := time.Now()
	if now.Sub(m.lastPaint) >= paintInterval {
		m.paintPending = false
		m.refreshViewport()
		return nil
	}
	if m.paintPending {
		return nil
	}
	m.paintPending = true
	wait := paintInterval - now.Sub(m.lastPaint)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return tea.Tick(wait, func(time.Time) tea.Msg { return paintDueMsg{} })
}

func (m model) applyStreamEvent(ev agent.Event) (model, tea.Cmd) {
	switch ev.Kind {
	case agent.EventPermission:
		m.pendingApproval = ev.Permission
		m.approvalDecision = ev.Decision
		m.status = "waiting for approval"
		m.upsertThinkingPlaceholder()
		return m, m.schedulePaint(true)
	case agent.EventToken:
		m.gotToken = true
		m.ensureStream().WriteString(ev.Text)
		cur := m.stream.String()
		// Sample head for noise check so large answers aren't scanned fully each batch.
		check := cur
		if len(check) > 1200 {
			check = check[:1200]
		}
		if m.verbose || !isMostlyToolNoise(check) {
			m.clearThinkingPlaceholder()
			m.upsertStreamingAssistant(cur)
		} else {
			// Keep status only; drop any partial tool-JSON bubble.
			m.dropStreamingAssistant()
			m.upsertThinkingPlaceholder()
		}
		m.status = fmt.Sprintf("streaming… %s", spinnerFrame())
		return m, m.schedulePaint(false)

	case agent.EventToolStart:
		m.gotToken = true
		if m.verbose {
			m.flushStreamAsLine()
			m.lines = append(m.lines, chatLine{
				role: "tool",
				text: formatToolStart(ev.Tool, ev.Text, true),
			})
		} else {
			m.dropStreamIfNoise()
		}
		m.status = formatToolStart(ev.Tool, ev.Text, false)
		m.upsertThinkingPlaceholder()
		return m, m.schedulePaint(true)

	case agent.EventToolEnd:
		m.pendingApproval = nil
		m.approvalDecision = nil
		line := formatToolEnd(ev.Tool, ev.ToolOut, m.verbose)
		if m.verbose {
			m.lines = append(m.lines, chatLine{role: "tool", text: line})
		}
		if !m.verbose && strings.HasPrefix(strings.TrimSpace(ev.ToolOut), "error:") {
			if !isBenignToolFailure(ev.Tool, ev.ToolOut) {
				m.lines = append(m.lines, chatLine{role: "error", text: line})
			}
		}
		m.status = line
		m.upsertThinkingPlaceholder()
		return m, m.schedulePaint(true)

	case agent.EventStatus:
		if ev.Text != "" {
			m.status = ev.Text
		}
		// Status only: update thinking line if visible; skip full paint when streaming.
		if m.showingThinkingOnly() {
			m.upsertThinkingPlaceholder()
			return m, m.schedulePaint(true)
		}
		return m, nil

	case agent.EventUsage:
		m.tokenUsage = ev.Usage
		return m, nil

	case agent.EventError:
		m.clearThinkingPlaceholder()
		m.flushStreamAsLine()
		m.lines = append(m.lines, chatLine{role: "error", text: ev.Text})
		m.status = "error"
		return m, m.schedulePaint(true)

	case agent.EventDone:
		m.pendingApproval = nil
		m.approvalDecision = nil
		m.clearThinkingPlaceholder()
		if m.verbose {
			m.flushStreamAsLine()
		} else {
			m.flushStreamAsLineQuiet()
		}
		// Keep input disabled until streamClosedMsg. EventDone is emitted before
		// the worker closes its channel; reopening input here lets a fast second
		// prompt race the old worker's final close event.
		m.status = "finalizing…"
		return m, m.schedulePaint(true)
	}
	return m, nil
}

func (m model) handleSlash(text string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/help", "/h":
		m.lines = append(m.lines, chatLine{role: "system", text: helpText()})
	case "/clear":
		m.deps.Agent.Reset()
		m.lines = []chatLine{{role: "system", text: "history cleared"}}
	case "/status":
		st := m.deps.Summary + "\nstatus: " + m.status
		workspace := m.deps.Workspace
		if workspace == "" {
			workspace = mustCwd()
		}
		st += "\nworkspace: " + workspace
		if b, err := runGitStatusShort(workspace); err == nil && b != "" {
			st += "\ngit:\n" + b
		}
		m.lines = append(m.lines, chatLine{role: "system", text: st})
	case "/model", "/models":
		mod, cmd := m.handleModelCmd(parts)
		return mod, cmd
	case "/tools":
		if len(parts) < 2 {
			state := "on"
			if !m.deps.Agent.Cfg.EnableTools {
				state = "off"
			}
			m.lines = append(m.lines, chatLine{role: "system", text: "tools: " + state + "\nusage: /tools on | /tools off"})
		} else {
			switch strings.ToLower(parts[1]) {
			case "on", "true", "1", "enable":
				m.deps.Agent.Cfg.EnableTools = true
				m.lines = append(m.lines, chatLine{role: "system", text: "tools enabled (used only when the task needs them)"})
			case "off", "false", "0", "disable":
				m.deps.Agent.Cfg.EnableTools = false
				m.lines = append(m.lines, chatLine{role: "system", text: "tools disabled for this session (faster chat)"})
			default:
				m.lines = append(m.lines, chatLine{role: "error", text: "usage: /tools on | /tools off"})
			}
		}
	case "/verbose", "/v":
		m.verbose = true
		m.lines = append(m.lines, chatLine{role: "system", text: "verbose on — full tool args/output and model preambles"})
	case "/quiet":
		m.verbose = false
		m.lines = append(m.lines, chatLine{role: "system", text: "quiet mode — compact tools (default). /verbose for full traces"})
	case "/retry", "/regenerate", "/redo":
		if m.busy {
			m.lines = append(m.lines, chatLine{role: "error", text: "busy — wait or Esc cancel first"})
			m.refreshViewport()
			return m, nil
		}
		prev := m.deps.Agent.PopLastExchange()
		if prev == "" {
			m.lines = append(m.lines, chatLine{role: "error", text: "nothing to retry"})
			m.refreshViewport()
			return m, nil
		}
		// Drop last user+assistant lines from UI if present
		for len(m.lines) > 0 {
			r := m.lines[len(m.lines)-1].role
			if r == "user" || r == "assistant" || r == "tool" || r == "error" || r == "assistant-stream" {
				m.lines = m.lines[:len(m.lines)-1]
				if r == "user" {
					break
				}
				continue
			}
			break
		}
		m.lines = append(m.lines, chatLine{role: "system", text: "retrying…"})
		m.refreshViewport()
		return m.handleSubmit(prev)
	case "/save":
		id := ""
		if len(parts) >= 2 {
			id = parts[1]
		}
		path := m.sessionPath(id)
		var err error
		if path == "" {
			err = fmt.Errorf("invalid session id")
		} else {
			_, err = m.deps.Agent.SaveSessionPath(path)
		}
		if err != nil {
			m.lines = append(m.lines, chatLine{role: "error", text: "save: " + err.Error()})
		} else {
			m.lines = append(m.lines, chatLine{role: "system", text: "saved session → " + path})
		}
	case "/sessions", "/history":
		list, err := listWorkspaceSessions(m.sessionDir(), 15)
		if err != nil {
			m.lines = append(m.lines, chatLine{role: "error", text: err.Error()})
		} else if len(list) == 0 {
			m.lines = append(m.lines, chatLine{role: "system", text: "no sessions in " + m.sessionDir() + "\n/save to create one"})
		} else {
			m.lines = append(m.lines, chatLine{role: "system", text: "sessions:\n  " + strings.Join(list, "\n  ") + "\n\nLoad: /load <id>"})
		}
	case "/load":
		if len(parts) < 2 {
			m.lines = append(m.lines, chatLine{role: "system", text: "usage: /load <session-id>"})
		} else if path := m.sessionPath(parts[1]); path == "" {
			m.lines = append(m.lines, chatLine{role: "error", text: "invalid session id"})
		} else if err := m.deps.Agent.LoadSessionPath(path); err != nil {
			m.lines = append(m.lines, chatLine{role: "error", text: err.Error()})
		} else {
			m.lines = []chatLine{
				{role: "system", text: "loaded session " + parts[1]},
				{role: "system", text: m.deps.Summary},
			}
		}
	case "/compact":
		m.deps.Agent.CompactHistory()
		m.lines = append(m.lines, chatLine{role: "system", text: "compacted old tool payloads in history"})
	case "/plan":
		if len(parts) < 2 {
			state := "off"
			if m.deps.Agent.PlanMode {
				state = "on"
			}
			m.lines = append(m.lines, chatLine{role: "system", text: "plan mode: " + state + "\nusage: /plan on | /plan off"})
		} else {
			switch strings.ToLower(parts[1]) {
			case "on", "true", "1", "enable":
				m.deps.Agent.PlanMode = true
				m.lines = append(m.lines, chatLine{role: "system", text: "plan mode ON — outline steps only (no tools). /plan off to implement."})
			case "off", "false", "0", "disable":
				m.deps.Agent.PlanMode = false
				m.lines = append(m.lines, chatLine{role: "system", text: "plan mode OFF — tools available again"})
			default:
				m.lines = append(m.lines, chatLine{role: "error", text: "usage: /plan on | /plan off"})
			}
		}
	case "/edit", "/e":
		prev := m.deps.Agent.LastUserText()
		if prev == "" {
			m.lines = append(m.lines, chatLine{role: "error", text: "no previous user message to edit"})
		} else {
			_ = m.deps.Agent.PopLastExchange()
			for len(m.lines) > 0 {
				r := m.lines[len(m.lines)-1].role
				if r == "user" || r == "assistant" || r == "tool" || r == "error" || r == "assistant-stream" || r == "thinking" {
					m.lines = m.lines[:len(m.lines)-1]
					if r == "user" {
						break
					}
					continue
				}
				break
			}
			m.ta.SetValue(prev)
			m.ta.CursorEnd()
			m.lines = append(m.lines, chatLine{role: "system", text: "edit last prompt in the input box, then Enter to resend (Alt+Enter for newline)"})
		}
	case "/copy", "/yank":
		text := lastAssistantText(m.lines)
		if text == "" {
			m.lines = append(m.lines, chatLine{role: "error", text: "no assistant reply to copy"})
		} else if path, err := copyToClipboardOrFile(text); err != nil {
			m.lines = append(m.lines, chatLine{role: "error", text: "copy failed: " + err.Error()})
		} else {
			m.lines = append(m.lines, chatLine{role: "system", text: "copied last agent reply → " + path})
		}
	case "/undo":
		msg, err := tools.UndoLast()
		if err != nil {
			m.lines = append(m.lines, chatLine{role: "error", text: err.Error()})
		} else {
			m.lines = append(m.lines, chatLine{role: "system", text: msg})
		}
	case "/stop":
		if m.busy && m.cancel != nil {
			m.cancel()
			m.status = "cancelling… (/stop)"
			m.lines = append(m.lines, chatLine{role: "system", text: "stop requested"})
		} else {
			m.lines = append(m.lines, chatLine{role: "system", text: "nothing to stop (not busy)"})
		}
	default:
		m.lines = append(m.lines, chatLine{role: "error", text: "unknown command " + cmd + " — try /help"})
	}
	m.refreshViewport()
	return m, nil
}

func lastAssistantText(lines []chatLine) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].role == "assistant" && strings.TrimSpace(lines[i].text) != "" {
			return lines[i].text
		}
	}
	return ""
}

// copyToClipboardOrFile tries wl-copy/xclip/pbcopy; always writes ~/.agenterm/last_reply.txt.
func copyToClipboardOrFile(text string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".agenterm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "last_reply.txt")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	for _, try := range [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"pbcopy"},
	} {
		if _, err := exec.LookPath(try[0]); err != nil {
			continue
		}
		cmd := exec.Command(try[0], try[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return path + " + clipboard", nil
		}
	}
	return path + " (clipboard tool not found; file only)", nil
}

func helpText() string {
	return strings.TrimSpace(`
Commands:
  /help              This help
  /clear             Clear conversation
  /status            Provider · model · URL · workspace path · git
  /model             Interactive picker (Tab · Enter · Esc)
  /model <name>      Switch model by name
  /tools on|off      Toggle function tools
  /plan on|off       Plan-only mode (no tools)
  /quiet | /verbose  Compact tools vs full traces
  /retry             Regenerate last reply
  /edit              Edit last prompt & resend
  /copy              Copy last agent reply
  /undo              Revert last file write/str_replace
  /stop              Cancel in-flight reply
  /save [id]         Save session
  /sessions          List sessions
  /load <id>         Resume session
  /compact           Shrink tool history
  /quit              Exit

Mentions: @README.md  @internal/agent

Tools: repo_map, grep, fetch, git, str_replace, write_file, run_shell, …

Keys:
  Enter           Send
  Alt+Enter       Newline in prompt
  Esc / /stop     Cancel in-flight reply
  Ctrl+C          Cancel if busy; quit when idle
  Ctrl+L          Clear history
  PgUp / PgDn     Scroll chat (also Ctrl+U / Ctrl+D half-page)
  Ctrl+↑ / Ctrl+↓ Line scroll · Home / End jump
  Mouse wheel     Scroll chat
  ↑ / ↓ (busy)    Scroll while agent is working
`)
}

func mustCwd() string {
	c, err := os.Getwd()
	if err != nil {
		return "."
	}
	return c
}

// displayCwd returns the process working directory for the TUI (tools use this).
// Home is shortened to ~; maxW>0 truncates the middle for narrow terminals.
func displayCwd(maxW int) string {
	return displayCwdAt(mustCwd(), maxW)
}

func displayCwdAt(c string, maxW int) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if c == home {
			c = "~"
		} else if strings.HasPrefix(c, home+string(os.PathSeparator)) {
			c = "~" + c[len(home):]
		}
	}
	if maxW > 12 && len(c) > maxW {
		// keep head and tail: ~/proj…/agenterm
		keep := (maxW - 1) / 2
		if keep < 4 {
			keep = 4
		}
		c = c[:keep] + "…" + c[len(c)-(maxW-keep-1):]
	}
	return c
}

func runGitStatusShort(workspace string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "-sb")
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 800 {
		s = s[:800] + "…"
	}
	return s, nil
}

func (m model) sessionDir() string {
	if m.deps.SessionPath != "" {
		return filepath.Dir(m.deps.SessionPath)
	}
	workspace := m.deps.Workspace
	if workspace == "" {
		workspace = mustCwd()
	}
	return filepath.Join(workspace, ".bolt", "sessions")
}

func (m model) sessionPath(id string) string {
	if id == "" {
		return m.deps.SessionPath
	}
	workspace := m.deps.Workspace
	if workspace == "" {
		workspace = mustCwd()
	}
	path, err := agent.WorkspaceSessionPath(workspace, id)
	if err != nil {
		return ""
	}
	return path
}

func listWorkspaceSessions(dir string, limit int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 15
	}
	var out []string
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if !entries[i].IsDir() && filepath.Ext(entries[i].Name()) == ".json" {
			out = append(out, entries[i].Name())
		}
	}
	return out, nil
}

// handleModelCmd implements Grok-style mid-chat model switch and listing.
// /model or /model list opens an interactive picker: Tab cycles, Enter selects, Esc cancels.
// /model <name> still switches immediately.
func (m model) handleModelCmd(parts []string) (model, tea.Cmd) {
	cur := m.deps.Agent.Cfg.Model
	// /model  or  /model list  → interactive picker
	if len(parts) < 2 || strings.EqualFold(parts[1], "list") || strings.EqualFold(parts[1], "ls") {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ids, err := m.deps.Agent.Client.ListModels(ctx)
		if err != nil {
			m.lines = append(m.lines, chatLine{
				role: "error",
				text: fmt.Sprintf("could not list models: %v\ncurrent: %s\nusage: /model <name>", err, cur),
			})
			m.refreshViewport()
			return m, nil
		}
		if len(ids) == 0 {
			m.lines = append(m.lines, chatLine{
				role: "system",
				text: "no models reported by server\ncurrent: " + cur + "\nusage: /model <name>",
			})
			m.refreshViewport()
			return m, nil
		}
		idx := 0
		for i, id := range ids {
			if id == cur {
				idx = i
				break
			}
		}
		m.modelPick = &modelPicker{ids: ids, idx: idx}
		m.ta.Blur()
		m.status = "pick model · Tab next · Enter select · Esc cancel"
		m.lines = append(m.lines, chatLine{
			role: "system",
			text: fmt.Sprintf("Select a model (%d available)\n  Tab / ↓  next ·  Shift+Tab / ↑  prev ·  Enter  select ·  Esc  cancel\n  Or type: /model <name>", len(ids)),
		})
		m.refreshViewport()
		return m, nil
	}

	// /model <name> — tags may include ":" (e.g. qwen2.5-coder:32b); join rest.
	name := strings.TrimSpace(strings.Join(parts[1:], " "))
	if name == "" {
		m.lines = append(m.lines, chatLine{role: "error", text: "usage: /model <name>"})
		m.refreshViewport()
		return m, nil
	}
	return m.applyModelSelection(name), nil
}

func (m model) handleModelPickKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.modelPick
	if p == nil || len(p.ids) == 0 {
		m.modelPick = nil
		m.ta.Focus()
		return m, nil
	}
	n := len(p.ids)
	switch msg.String() {
	case "esc":
		m.modelPick = nil
		m.ta.Focus()
		m.status = "ready"
		m.lines = append(m.lines, chatLine{role: "system", text: "model pick cancelled (still: " + m.deps.Agent.Cfg.Model + ")"})
		m.refreshViewport()
		return m, nil
	case "ctrl+c":
		m.modelPick = nil
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "tab", "down", "j", "ctrl+n":
		p.idx = (p.idx + 1) % n
		m.status = fmt.Sprintf("model pick %d/%d · %s", p.idx+1, n, p.ids[p.idx])
		return m, nil
	case "shift+tab", "up", "k", "ctrl+p":
		p.idx = (p.idx - 1 + n) % n
		m.status = fmt.Sprintf("model pick %d/%d · %s", p.idx+1, n, p.ids[p.idx])
		return m, nil
	case "enter":
		name := p.ids[p.idx]
		m.modelPick = nil
		m.ta.Focus()
		return m.applyModelSelection(name), nil
	case "home", "g":
		p.idx = 0
		return m, nil
	case "end", "G":
		p.idx = n - 1
		return m, nil
	default:
		// Digits 1-9 quick-select first nine models
		if len(msg.String()) == 1 {
			ch := msg.String()[0]
			if ch >= '1' && ch <= '9' {
				i := int(ch - '1')
				if i < n {
					p.idx = i
					name := p.ids[p.idx]
					m.modelPick = nil
					m.ta.Focus()
					return m.applyModelSelection(name), nil
				}
			}
		}
		// Ignore other keys while picking (do not type into chat)
		return m, nil
	}
}

func (m model) applyModelSelection(name string) model {
	prev := m.deps.Agent.Cfg.Model
	m.deps.Agent.Cfg.Model = name
	m.deps.Summary = fmt.Sprintf("%s · %s · %s", m.deps.Agent.Cfg.Provider, m.deps.Agent.Cfg.Model, m.deps.Agent.Cfg.BaseURL)
	m.status = "model: " + name
	msg := fmt.Sprintf("model set to %s", name)
	if prev != "" && prev != name {
		msg = fmt.Sprintf(
			"model: %s → %s\n"+
				"Next message uses the new model.\n"+
				"Note: Ollama often unloads the old model and loads the new one on first request —\n"+
				"32B models can sit on “thinking…” for 30s–several minutes with no tokens yet.\n"+
				"Status bar shows a wait timer; Esc cancels. Tip: /clear if chat history is huge.",
			prev, name,
		)
	}
	m.lines = append(m.lines, chatLine{role: "system", text: msg})
	m.refreshViewport()
	return m
}

// modelPickerView renders the selectable model list for the TUI.
func (m model) modelPickerView(width int) string {
	p := m.modelPick
	if p == nil {
		return ""
	}
	cur := m.deps.Agent.Cfg.Model
	var b strings.Builder
	b.WriteString(styleHeader.Render(" Select model ") + styleStatus.Render("Tab cycle · Enter select · Esc cancel"))
	b.WriteByte('\n')
	// Show a window of models around the cursor for long lists.
	const window = 12
	start := 0
	if len(p.ids) > window {
		start = p.idx - window/2
		if start < 0 {
			start = 0
		}
		if start+window > len(p.ids) {
			start = len(p.ids) - window
		}
	}
	end := start + window
	if end > len(p.ids) {
		end = len(p.ids)
	}
	if start > 0 {
		b.WriteString(styleStatus.Render(fmt.Sprintf("  … %d more above\n", start)))
	}
	for i := start; i < end; i++ {
		id := p.ids[i]
		line := id
		if id == cur {
			line = id + "  (current)"
		}
		if i == p.idx {
			// Highlighted selection
			b.WriteString(styleAsst.Render("❯ " + line))
		} else if id == cur {
			b.WriteString(styleStatus.Render("* " + line))
		} else {
			b.WriteString(styleStatus.Render("  " + line))
		}
		b.WriteByte('\n')
	}
	if end < len(p.ids) {
		b.WriteString(styleStatus.Render(fmt.Sprintf("  … %d more below\n", len(p.ids)-end)))
	}
	b.WriteString(styleHelp.Render(fmt.Sprintf("\n[%d/%d]  %s", p.idx+1, len(p.ids), p.ids[p.idx])))
	return styleBox.Width(max(10, width-2)).BorderForeground(colorAccent).Render(strings.TrimRight(b.String(), "\n"))
}

func (m *model) upsertStreamingAssistant(content string) {
	if m.validTurnAssistant() {
		m.lines[m.turnAssistant].role = "assistant-stream"
		m.lines[m.turnAssistant].text = content
		return
	}
	m.lines = append(m.lines, chatLine{role: "assistant-stream", text: content})
	m.turnAssistant = len(m.lines) - 1
}

func (m *model) flushStreamAsLine() {
	sb := m.ensureStream()
	if sb.Len() == 0 {
		if m.validTurnAssistant() && m.lines[m.turnAssistant].role == "assistant-stream" {
			m.lines[m.turnAssistant].role = "assistant"
		}
		return
	}
	content := sb.String()
	sb.Reset()
	if m.validTurnAssistant() {
		m.lines[m.turnAssistant] = chatLine{role: "assistant", text: content}
		return
	}
	m.lines = append(m.lines, chatLine{role: "assistant", text: content})
	m.turnAssistant = len(m.lines) - 1
}

// flushStreamAsLineQuiet drops tool-JSON / empty stream; keeps real answers.
func (m *model) flushStreamAsLineQuiet() {
	sb := m.ensureStream()
	content := strings.TrimSpace(sb.String())
	sb.Reset()
	if content == "" {
		m.dropStreamingAssistant()
		return
	}
	if isMostlyToolNoise(content) {
		// Tool dumps (incl. long run_shell crawls) never become the final "Agent" answer.
		if cleaned := stripLeadingToolNoise(content); cleaned != "" && !isMostlyToolNoise(cleaned) {
			content = cleaned
		} else {
			m.dropStreamingAssistant()
			return
		}
	}
	if m.validTurnAssistant() {
		m.lines[m.turnAssistant] = chatLine{role: "assistant", text: content}
		return
	}
	m.lines = append(m.lines, chatLine{role: "assistant", text: content})
	m.turnAssistant = len(m.lines) - 1
}

// stripLeadingToolNoise removes a leading JSON/tool dump if prose remains after it.
func stripLeadingToolNoise(s string) string {
	s = strings.TrimSpace(s)
	// fenced ```json ... ```
	if i := strings.Index(s, "```"); i >= 0 && i < 80 {
		rest := s[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			after := strings.TrimSpace(rest[j+3:])
			if after != "" && !isMostlyToolNoise(after) {
				return after
			}
		}
	}
	if i := strings.Index(s, "{"); i >= 0 && i < 120 {
		// find matching-ish end of first object (best-effort)
		depth := 0
		for k := i; k < len(s); k++ {
			switch s[k] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					after := strings.TrimSpace(s[k+1:])
					if len(after) > 40 && !isMostlyToolNoise(after) {
						return after
					}
					return ""
				}
			}
		}
	}
	return ""
}

func (m *model) dropStreamingAssistant() {
	if m.validTurnAssistant() && m.lines[m.turnAssistant].role == "assistant-stream" {
		m.lines = append(m.lines[:m.turnAssistant], m.lines[m.turnAssistant+1:]...)
		m.turnAssistant = -1
	}
}

func (m *model) validTurnAssistant() bool {
	return m.turnAssistant >= 0 && m.turnAssistant < len(m.lines)
}

// upsertThinkingPlaceholder keeps a blinking "Thinking…" line in the chat while busy
// so quiet mode (hidden tool JSON) does not look frozen.
func (m *model) upsertThinkingPlaceholder() {
	if !m.busy {
		return
	}
	// Real non-empty assistant stream already visible — no placeholder.
	if m.validTurnAssistant() {
		last := m.lines[m.turnAssistant]
		if (last.role == "assistant-stream" || last.role == "assistant") &&
			strings.TrimSpace(last.text) != "" && !strings.HasPrefix(strings.TrimSpace(last.text), "Thinking") {
			return
		}
	}
	text := m.busyBannerText()
	if m.validTurnAssistant() {
		m.lines[m.turnAssistant].role = "assistant-stream"
		m.lines[m.turnAssistant].text = text
		return
	}
	m.lines = append(m.lines, chatLine{role: "assistant-stream", text: text})
	m.turnAssistant = len(m.lines) - 1
}

func (m model) busyBannerText() string {
	dots := []string{"   ", ".  ", ".. ", "..."}
	frame := dots[int(time.Now().UnixNano()/2e8)%len(dots)]
	sec := m.waitSecs
	if sec < 0 {
		sec = 0
	}
	text := fmt.Sprintf("Thinking%s  %ds  %s", frame, sec, spinnerFrame())
	st := strings.TrimSpace(m.status)
	if st != "" && st != "ready" && !strings.HasPrefix(st, "Thinking") {
		text = fmt.Sprintf("%s\n%s", text, truncate(st, 96))
	} else if m.deps.Agent != nil && m.deps.Agent.Cfg.Model != "" {
		text = fmt.Sprintf("%s\nwaiting on %s · Esc cancel", text, m.deps.Agent.Cfg.Model)
	}
	return text
}

func (m *model) clearThinkingPlaceholder() {
	for len(m.lines) > 0 && m.lines[len(m.lines)-1].role == "thinking" {
		m.lines = m.lines[:len(m.lines)-1]
	}
}

func (m *model) dropStreamIfNoise() {
	sb := m.ensureStream()
	content := strings.TrimSpace(sb.String())
	sb.Reset()
	m.dropStreamingAssistant()
	// Keep non-noise preamble only in verbose (caller already branched).
	_ = content
}

// isMostlyToolNoise detects model dumps of tool-call JSON and short “let me tool” chatter.
// Must stay conservative: false positives hide real answers (looks like a hung "ready" turn).
func isMostlyToolNoise(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	lowAll := strings.ToLower(s)

	// Model dumps tool invocation as chat (any length) — especially run_shell crawls.
	if looksLikeToolInvocationDump(s, lowAll) {
		return true
	}

	// XML / tag-style tool dumps (any length — not user prose)
	for _, tag := range []string{
		"<tool_call", "</tool_call>", "<function_call", "<parameter",
		`"tool_calls"`,
	} {
		if strings.Contains(lowAll, tag) {
			// Long answers after a tool tag still count as noise for live paint;
			// flushStreamAsLineQuiet will keep long text via stripLeadingToolNoise.
			if len(s) < 400 || strings.Count(s, "{") >= 1 {
				return true
			}
		}
	}

	// Tool-shaped JSON is the majority of the message
	if strings.Contains(s, `"name"`) && (strings.Contains(s, `"arguments"`) || strings.Contains(s, `"parameters"`)) {
		withoutSpace := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\t' {
				return -1
			}
			return r
		}, s)
		if strings.HasPrefix(withoutSpace, "{") || strings.HasPrefix(withoutSpace, "[") {
			return true
		}
		if i := strings.Index(s, "{"); i >= 0 {
			jsonPart := s[i:]
			if len(jsonPart) > len(s)*2/3 {
				return true
			}
		}
		// Short "Let's read…\n{json}" preambles
		if len(s) < 350 {
			return true
		}
	}

	// Almost pure JSON blob (streaming tool args), short only
	if len(s) < 600 && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
		if strings.Count(s, `"`) >= 2 && !strings.Contains(s, "\n\n") {
			return true
		}
	}

	// Short tool-intent chatter only — never match long answers (e.g. "I need to…").
	if len(s) < 160 {
		for _, p := range []string{
			"let's read", "lets read", "i'll read", "i will read",
			"let me read", "i'll use", "i will use",
			"using the read_file", "call read_file",
			"let me check", "let me look",
		} {
			if strings.Contains(lowAll, p) {
				return true
			}
		}
		// Bare "find_files README.md" style tool sketch with no real prose
		if looksLikeBareToolSketch(lowAll) {
			return true
		}
	}
	return false
}

// looksLikeBareToolSketch matches short lines like "find_files README.md" with no sentence.
func looksLikeBareToolSketch(low string) bool {
	tools := []string{
		"find_files", "read_file", "write_file", "str_replace", "list_dir",
		"run_shell", "run_tests", "grep", "fetch",
	}
	for _, t := range tools {
		if strings.HasPrefix(low, t) || strings.HasPrefix(strings.TrimLeft(low, "`* "), t) {
			// no multi-sentence prose
			if !strings.Contains(low, ". ") && len(low) < 120 {
				return true
			}
		}
	}
	return false
}

// looksLikeToolInvocationDump matches prose-less tool dumps (often shown as "Agent" answer).
func looksLikeToolInvocationDump(s, low string) bool {
	// run_shell {"command": "..."}  or  → run_shell · ...
	if strings.HasPrefix(low, "run_shell") || strings.HasPrefix(low, "→ run_shell") {
		return true
	}
	if strings.Contains(low, `run_shell`) && strings.Contains(s, `"command"`) {
		return true
	}
	if strings.HasPrefix(low, "fetch ") || strings.HasPrefix(low, "fetch{") || strings.HasPrefix(low, `fetch {`) {
		return true
	}
	// Bare shell pipelines the model pastes instead of calling tools
	if looksLikeShellOnlyMessage(s, low) {
		return true
	}
	// Markdown fence that is only a shell one-liner
	trim := strings.TrimSpace(s)
	if strings.HasPrefix(trim, "```") {
		body := strings.TrimSpace(strings.TrimPrefix(trim, "```"))
		for _, lang := range []string{"bash", "sh", "shell", "zsh", "console"} {
			if strings.HasPrefix(strings.ToLower(body), lang) {
				body = strings.TrimSpace(body[len(lang):])
				break
			}
		}
		if strings.Contains(body, "```") {
			body = strings.TrimSpace(body[:strings.Index(body, "```")])
		}
		if looksLikeShellOnlyMessage(body, strings.ToLower(body)) {
			return true
		}
	}
	return false
}

// looksLikeShellOnlyMessage is true when the whole reply is a shell recipe, not prose.
func looksLikeShellOnlyMessage(s, low string) bool {
	s = strings.TrimSpace(s)
	low = strings.TrimSpace(low)
	if s == "" {
		return true
	}
	// multi-paragraph prose → not shell-only
	if strings.Count(s, "\n\n") >= 1 && len(s) > 200 {
		// still noise if every non-empty line looks like shell
		lines := strings.Split(s, "\n")
		shellLines, textLines := 0, 0
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "```") {
				continue
			}
			ll := strings.ToLower(ln)
			if lineLooksLikeShell(ll) {
				shellLines++
			} else if len(ln) > 20 {
				textLines++
			}
		}
		if shellLines > 0 && textLines == 0 {
			return true
		}
		return false
	}
	// single line / short block
	if lineLooksLikeShell(low) {
		return true
	}
	if strings.Contains(low, "xargs") {
		return true
	}
	if strings.Contains(low, "https?:") && strings.Contains(low, "grep") && strings.Contains(low, "|") {
		return true
	}
	return false
}

func lineLooksLikeShell(low string) bool {
	low = strings.TrimSpace(low)
	if low == "" {
		return false
	}
	// strip leading $ or # shell prompts
	low = strings.TrimLeft(low, "$ #")
	low = strings.TrimSpace(low)
	prefixes := []string{
		"grep ", "rg ", "find ", "xargs ", "curl ", "wget ", "awk ", "sed ",
		"find_files", "run_shell", "bash ", "sh ", "make ", "git ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	// pipeline-heavy without sentence punctuation
	if strings.Count(low, "|") >= 1 && !strings.Contains(low, ". ") {
		if strings.Contains(low, "grep") || strings.Contains(low, "xargs") ||
			strings.Contains(low, "curl") || strings.Contains(low, "wget") ||
			strings.Contains(low, "sort") {
			return true
		}
	}
	return false
}

func formatToolStart(name, args string, verbose bool) string {
	if verbose {
		return fmt.Sprintf("→ %s(%s)", name, truncate(args, 200))
	}
	path := toolArgHint(args)
	if path != "" {
		return fmt.Sprintf("→ %s · %s", name, truncate(path, 72))
	}
	return fmt.Sprintf("→ %s", name)
}

// isBenignToolFailure is true for expected network/local failures that should not
// spam the chat as red Error lines (final link-check report covers them).
func isBenignToolFailure(tool, out string) bool {
	low := strings.ToLower(out)
	if tool == "fetch" {
		if strings.Contains(low, "connection refused") ||
			strings.Contains(low, "no such host") ||
			strings.Contains(low, "i/o timeout") ||
			strings.Contains(low, "timeout") ||
			strings.Contains(low, "certificate") ||
			strings.Contains(low, "tls") ||
			strings.Contains(low, "eof") {
			return true
		}
	}
	return false
}

func formatToolEnd(name, out string, verbose bool) string {
	if strings.HasPrefix(out, "error:") {
		return fmt.Sprintf("← %s · %s", name, truncate(out, 160))
	}
	if verbose {
		return fmt.Sprintf("← %s\n%s", name, truncate(out, 900))
	}
	n := len(out)
	unit := "B"
	sz := float64(n)
	if n >= 1024 {
		sz = float64(n) / 1024
		unit = "KB"
	}
	// One-line confirmation; model will summarize in the final answer.
	return fmt.Sprintf("← %s · ok (%.1f %s)", name, sz, unit)
}

func toolArgHint(argsJSON string) string {
	argsJSON = strings.TrimSpace(argsJSON)
	if argsJSON == "" {
		return ""
	}
	// Prefer useful keys for status bar hints
	for _, key := range []string{"url", "path", "name", "root", "command", "pattern"} {
		// cheap extract: "path": "..."
		needle := `"` + key + `"`
		i := strings.Index(argsJSON, needle)
		if i < 0 {
			continue
		}
		rest := argsJSON[i+len(needle):]
		// skip : and spaces
		rest = strings.TrimLeft(rest, " \t\n:")
		if len(rest) == 0 || rest[0] != '"' {
			continue
		}
		rest = rest[1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		return rest[:j]
	}
	return truncate(argsJSON, 48)
}

// chatBubble paints a full-width message block so light-grey vs white (or
// dark-slate variants) separates user questions from agent answers.
// Large bodies skip lipgloss Width (O(lines) and freezes the TUI on big answers).
func chatBubble(base lipgloss.Style, label, body string, width int) string {
	if width < 10 {
		width = 10
	}
	if len(body) > bubbleMaxBytes {
		// Fast path: label strip + plain body (still scrollable).
		return base.Width(width).Render(label) + "\n" + body
	}
	return base.Width(width).Render(label + "\n" + body)
}

func (m *model) renderAssistantBody(ln *chatLine, width int) string {
	if ln.role == "assistant-stream" || m.renderer == nil || ln.text == "" {
		return wrap(ln.text, width-4)
	}
	// Reuse cached glamour output when text is unchanged.
	if ln.mdCache != "" && ln.mdSrc == ln.text {
		return ln.mdCache
	}
	// Huge markdown through glamour blocks the event loop for seconds.
	if len(ln.text) > glamourMaxBytes {
		out := wrap(ln.text, width-4)
		ln.mdCache = out
		ln.mdSrc = ln.text
		return out
	}
	if out, err := m.renderer.Render(ln.text); err == nil {
		out = strings.TrimRight(out, "\n")
		ln.mdCache = out
		ln.mdSrc = ln.text
		return out
	}
	return wrap(ln.text, width-4)
}

func (m *model) refreshViewport() {
	var b strings.Builder
	width := m.vp.Width
	if width < 20 {
		width = 80
	}
	for i := range m.lines {
		ln := &m.lines[i]
		switch ln.role {
		case "user":
			label := styleUser.Background(bgUser).Render("You")
			body := wrap(ln.text, width-4)
			b.WriteString(chatBubble(styleUserBubble, label, body, width) + "\n\n")
		case "assistant", "assistant-stream":
			labelText := "Agent"
			if ln.role == "assistant-stream" {
				labelText = "Agent …"
			}
			label := styleAsst.Background(bgAsst).Render(labelText)
			rendered := m.renderAssistantBody(ln, width)
			b.WriteString(chatBubble(styleAsstBubble, label, rendered, width) + "\n\n")
		case "thinking":
			label := styleAsst.Background(bgAsst).Render("Agent")
			body := styleStatus.Background(bgAsst).Render(wrap(ln.text, width-4))
			b.WriteString(chatBubble(styleAsstBubble, label, body, width) + "\n\n")
		case "tool":
			b.WriteString(styleTool.Render("Tool") + "\n")
			b.WriteString(styleTool.Render(wrap(ln.text, width)) + "\n\n")
		case "error":
			b.WriteString(styleErr.Render("Error") + "\n")
			b.WriteString(styleErr.Render(wrap(ln.text, width)) + "\n\n")
		default:
			b.WriteString(styleStatus.Render(wrap(ln.text, width)) + "\n\n")
		}
	}
	// Stick to bottom only when already following the latest lines. If the user
	// scrolled up to read a long answer, keep their YOffset across refreshes
	// (streaming / busy ticks would otherwise yank them back down).
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(b.String())
	if atBottom {
		m.vp.GotoBottom()
	}
	m.lastPaint = time.Now()
}

// handleChatScrollKey scrolls the transcript for pager-style keys. Returns true
// when the key was consumed so the textarea does not also handle it.
func (m *model) handleChatScrollKey(msg tea.KeyMsg) bool {
	km := m.vp.KeyMap
	switch {
	case key.Matches(msg, km.PageUp):
		m.vp.PageUp()
		return true
	case key.Matches(msg, km.PageDown):
		m.vp.PageDown()
		return true
	case key.Matches(msg, km.HalfPageUp):
		m.vp.HalfPageUp()
		return true
	case key.Matches(msg, km.HalfPageDown):
		m.vp.HalfPageDown()
		return true
	case key.Matches(msg, km.Up):
		m.vp.ScrollUp(1)
		return true
	case key.Matches(msg, km.Down):
		m.vp.ScrollDown(1)
		return true
	case msg.String() == "home", msg.String() == "ctrl+home":
		m.vp.GotoTop()
		return true
	case msg.String() == "end", msg.String() == "ctrl+end":
		m.vp.GotoBottom()
		return true
	}
	return false
}

func (m *model) cyclePermissionMode() tea.Cmd {
	mode := permissions.ModeAsk
	switch m.permissionMode {
	case permissions.ModeAsk:
		mode = permissions.ModeAllow
	case permissions.ModeAllow:
		mode = permissions.ModePlan
	default:
		mode = permissions.ModeAsk
	}
	m.permissionMode = mode
	if m.deps.Agent.Permissions != nil {
		m.deps.Agent.Permissions.SetMode(mode)
	}
	m.status = "permission mode: " + strings.ToUpper(string(mode))
	return nil
}

func (m *model) toggleMouseMode() tea.Cmd {
	if m.mouseMode == "SELECT" {
		m.mouseMode = "INTERACTIVE"
		m.status = "mouse mode: INTERACTIVE"
		return tea.EnableMouseCellMotion
	}
	m.mouseMode = "SELECT"
	m.status = "mouse mode: SELECT"
	return tea.DisableMouse
}

func (m *model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.approvalDecision == nil {
		return m, nil
	}
	decision := permissions.Decision("")
	switch msg.String() {
	case "1", "y", "enter":
		decision = permissions.AllowOnce
	case "2":
		decision = permissions.AllowSession
	case "3":
		decision = permissions.AllowPermanent
	case "4", "n", "esc":
		decision = permissions.Deny
	default:
		return m, nil
	}
	m.approvalDecision <- decision
	m.pendingApproval = nil
	m.approvalDecision = nil
	m.status = "approved"
	return m, nil
}

func approvalText(r *permissions.Request) string {
	if r == nil {
		return "Approval required"
	}
	return fmt.Sprintf("Approval required\nTool: %s\nAction: %s\nLevel: %s\n\n1 Allow once  2 Allow session  3 Allow permanently  4 Deny", r.Tool, r.Arguments, strings.ToUpper(string(r.Level)))
}

func todoText() string {
	items := todos.Global.Items()
	if len(items) == 0 {
		return "Todos\n(no todos)"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Todos (%d open)\n", countOpenTodos(items)))
	for _, item := range items {
		mark := "[ ]"
		if item.Completed {
			mark = "[x]"
		}
		b.WriteString(fmt.Sprintf("%s %s %s\n", mark, item.ID, item.Description))
	}
	return strings.TrimRight(b.String(), "\n")
}

func countOpenTodos(items []todos.Item) int {
	n := 0
	for _, item := range items {
		if !item.Completed {
			n++
		}
	}
	return n
}

func commandsText() string {
	return "Ctrl+Q  Quit\nCtrl+R  Permission\nCtrl+L  Mouse\nCtrl+Y  Vision\nCtrl+T  Todos\nCtrl+O  Commands\nCtrl+B  Theme\n\nEnter       Send\nShift+Enter Newline"
}

func applyTheme(name string) {
	if name == "black" {
		styleHeader = lipgloss.NewStyle().Foreground(lipgloss.Color("#67e8f9")).Bold(true).Padding(0, 1)
		styleBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#334155")).Padding(0, 1)
		return
	}
	styleHeader = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Padding(0, 1)
	styleBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(0, 1)
}

func (m model) View() string {
	w := m.width
	if w == 0 {
		w = 80
	}
	body := styleBox.Width(max(10, w-2)).Render(m.vp.View())
	status := m.status
	if m.busy && status == "ready" {
		status = "thinking"
	}
	if status != "" {
		status = strings.ToUpper(status[:1]) + status[1:]
	}
	modelName := m.deps.Agent.Cfg.Provider + "/" + m.deps.Agent.Cfg.Model
	tokens := "—"
	if m.tokenUsage != nil {
		total := m.tokenUsage.TotalTokens
		if total == 0 {
			total = m.tokenUsage.PromptTokens + m.tokenUsage.CompletionTokens
		}
		if total > 0 {
			tokens = fmt.Sprintf("%d", total)
		}
	}
	footer := fmt.Sprintf("Bolt | Permission Mode: %s | Mouse Mode: %s | Vision: %s | Model: %s | Tokens: %s | %s", strings.ToUpper(string(m.permissionMode)), m.mouseMode, map[bool]string{true: "ON", false: "OFF"}[m.visionEnabled], modelName, tokens, status)
	statusLine := styleStatus.Render(wrap(footer, max(20, w-4)))
	help := styleHelp.Render("Ctrl+Q quit · Ctrl+R permission · Ctrl+L mouse · Ctrl+Y vision · Ctrl+T todos · Ctrl+O commands · Ctrl+B theme · Enter send · Shift+Enter newline")
	if m.deps.Agent != nil && m.deps.Agent.PlanMode {
		help = styleHelp.Render("PLAN MODE · Ctrl+Q quit · Ctrl+R permission · Enter send · Shift+Enter newline")
	}
	if m.modelPick != nil {
		help = styleHelp.Render("Tab / ↓ next · Shift+Tab / ↑ prev · Enter select · Esc cancel · 1-9 quick")
	}
	// Always show workspace path above the prompt so you know where tools write.
	workspace := m.deps.Workspace
	if workspace == "" {
		workspace = mustCwd()
	}
	cwdLine := styleHelp.Render("📁 " + displayCwdAt(workspace, max(20, w-8)))
	input := styleBox.Width(max(10, w-2)).Render(m.ta.View())
	parts := []string{body, statusLine}
	if m.pendingApproval != nil {
		parts = append(parts, styleBox.Width(max(10, w-2)).Render(approvalText(m.pendingApproval)))
	}
	if m.todosOpen {
		parts = append(parts, styleBox.Width(max(10, w-2)).Render(todoText()))
	}
	if m.commandsOpen {
		parts = append(parts, styleBox.Width(max(10, w-2)).Render(commandsText()))
	}
	if m.modelPick != nil {
		parts = append(parts, m.modelPickerView(w))
	}
	parts = append(parts, cwdLine, input, help)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func wrap(s string, width int) string {
	if width < 20 {
		return s
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		for len(line) > width {
			b.WriteString(line[:width])
			b.WriteByte('\n')
			line = line[width:]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func spinnerFrame() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[int(time.Now().UnixNano()/1e8)%len(frames)]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Run launches the full-screen TUI.
func Run(deps Deps) error {
	// Discourage libraries from probing terminal fg/bg via OSC (leaks into stdin).
	// Fixed styles above are the main fix; these env hints help termenv/glamour.
	if os.Getenv("GLAMOUR_STYLE") == "" {
		_ = os.Setenv("GLAMOUR_STYLE", "dark")
	}
	p := tea.NewProgram(
		New(deps),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}
