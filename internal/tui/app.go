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
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	glamourstyles "github.com/charmbracelet/glamour/styles"
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
	colorMuted      = lipgloss.Color("#94a3b8")
	colorAccent     = lipgloss.Color("#38bdf8")
	colorUser       = lipgloss.Color("#a78bfa")
	colorAsst       = lipgloss.Color("#4ade80")
	colorTool       = lipgloss.Color("#fbbf24")
	colorError      = lipgloss.Color("#f87171")
	colorBorder     = lipgloss.Color("#1e293b")
	colorBackground = lipgloss.Color("#0f172a")
	colorForeground = lipgloss.Color("#e2e8f0")

	// Bubble backgrounds: light grey (You) vs white/slate (Agent) so Q/A
	// are distinct on light terminals; AdaptiveColor keeps dark terminals readable.
	bgUser = lipgloss.AdaptiveColor{Light: "#e5e7eb", Dark: "#1e293b"} // light grey / slate
	bgAsst = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0f172a"} // white / near-black
	fgBody = lipgloss.AdaptiveColor{Light: "#1e293b", Dark: "#e2e8f0"}

	styleHeader = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	// Chrome is foreground-only so unused terminal space stays quiet.
	styleStatus = lipgloss.NewStyle().Foreground(colorMuted)
	styleHelp   = lipgloss.NewStyle().Foreground(colorMuted)
	styleUser   = lipgloss.NewStyle().Foreground(colorUser).Bold(true)
	styleAsst   = lipgloss.NewStyle().Foreground(colorAsst).Bold(true)
	styleTool   = lipgloss.NewStyle().Foreground(colorTool)
	styleErr    = lipgloss.NewStyle().Foreground(colorError)
	// Dialogs/overlays keep a light border; input does not use this.
	styleBox = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(colorBorder).Padding(0, 1)
	// Conversation and message bodies sit on the terminal background.
	styleConversation = lipgloss.NewStyle().Foreground(fgBody)
	styleUserBubble   = lipgloss.NewStyle().Foreground(fgBody)
	styleAsstBubble   = lipgloss.NewStyle().Foreground(fgBody)
	styleTodo         = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(colorBorder).Foreground(fgBody).Padding(0, 1)
	styleRoot         = lipgloss.NewStyle().Foreground(colorForeground)
)

const todoSideThreshold = 90

type tuiLayout struct {
	width, height      int
	headerHeight       int
	conversationHeight int
	statusHeight       int
	inputHeight        int
	workspaceHeight    int
	footerHeight       int
	todoWidth          int
	conversationWidth  int
}

// layoutRect describes a row range in the final terminal frame. Bottom is
// exclusive, matching the usual [top, bottom) rectangle convention.
type layoutRect struct {
	top, bottom int
}

func (r layoutRect) height() int { return max(0, r.bottom-r.top) }

type layoutRects struct {
	conversation, todo, status, input, footer layoutRect
}

// contentMaxWidth caps readable conversation/prose width so wide terminals
// keep quiet margins instead of stretching every message edge-to-edge.
func contentMaxWidth(avail int) int {
	if avail < 40 {
		return max(20, avail)
	}
	cap := 120
	if avail >= 160 {
		cap = 130
	} else if avail < 100 {
		cap = 100
	}
	if avail < cap {
		return avail
	}
	return cap
}

func todoSideWidth(width int) int {
	if width < todoSideThreshold {
		return 0
	}
	if width < 120 {
		return 24
	}
	return 30
}

func todoPanelHeight() int {
	return strings.Count(todoText(), "\n") + 3
}

func (m model) conversationWidth(width int) int {
	if side := todoSideWidth(width); m.todosOpen && side > 0 {
		return max(20, width-side-1)
	}
	return max(20, width)
}

func (m model) messageWidth() int {
	return contentMaxWidth(m.vp.Width)
}

func (m model) todoOnSide(width int) bool {
	return m.todosOpen && todoSideWidth(width) > 0
}

func inputHeightFor(ta textarea.Model, width int) int {
	// Keep a one-line prompt compact, while allowing intentional multiline input
	// to grow without stealing the whole terminal from the conversation.
	lines := strings.Count(ta.Value(), "\n") + 1
	if width > 0 {
		promptWidth := lipgloss.Width(ta.Prompt)
		for _, line := range strings.Split(ta.Value(), "\n") {
			lines += max(0, (lipgloss.Width(line)+max(1, width-promptWidth)-1)/max(1, width-promptWidth)-1)
		}
	}
	if lines < 1 {
		lines = 1
	}
	return min(4, lines)
}

func (m model) layoutFor(width, height int) tuiLayout {
	if width < 1 {
		width = 80
	}
	if height < 1 {
		height = 24
	}
	l := tuiLayout{
		width: width, height: height,
		headerHeight: 1, statusHeight: 1, workspaceHeight: 1, footerHeight: 1,
		todoWidth: 0,
	}
	l.inputHeight = inputHeightFor(m.ta, max(1, width-2))
	if m.todosOpen && todoSideWidth(width) > 0 {
		l.todoWidth = todoSideWidth(width)
	}
	l.conversationWidth = max(1, width-l.todoWidth)
	fixed := l.headerHeight + l.statusHeight + l.inputHeight + l.workspaceHeight + l.footerHeight
	fixed += m.optionalPanelHeight(width)
	// Fixed regions own the bottom of the frame. When optional panels consume
	// the whole terminal there may be no conversation rows left; allowing zero
	// here keeps the model frame within the terminal instead of relying on the
	// Bubble Tea renderer to truncate it from the top.
	l.conversationHeight = max(0, height-fixed)
	return l
}

func (m model) optionalPanelHeight(width int) int {
	height := 0
	if m.commandsOpen {
		height += lipgloss.Height(styleBox.Width(max(10, width)).Render(commandsText()))
	}
	if m.pendingApproval != nil {
		height += lipgloss.Height(styleBox.Width(max(10, width)).Render(approvalText(m.pendingApproval)))
	}
	if m.modelPick != nil {
		height += lipgloss.Height(m.modelPickerView(width + 2))
	}
	return height
}

func (m model) rectsFor(l tuiLayout) layoutRects {
	conversationTop := l.headerHeight
	statusTop := conversationTop + l.conversationHeight
	inputTop := statusTop + l.statusHeight + m.optionalPanelHeight(l.width) + l.workspaceHeight
	return layoutRects{
		conversation: layoutRect{conversationTop, statusTop},
		todo:         layoutRect{conversationTop, statusTop},
		status:       layoutRect{statusTop, statusTop + l.statusHeight},
		input:        layoutRect{inputTop, inputTop + l.inputHeight},
		footer:       layoutRect{inputTop + l.inputHeight, l.height},
	}
}

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
	// turnFailed keeps a provider/tool error visible through EventDone and the
	// final stream-close event. It is reset when the next turn starts.
	turnFailed bool
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
	// modelDiscoveryLoading owns the asynchronous /model request while the
	// existing picker is still closed. A generation prevents late results from
	// reopening the picker after cancellation or a newer request.
	modelDiscoveryLoading bool
	modelDiscoverySeq     uint64
	modelDiscoveryCancel  context.CancelFunc
	// paintThrottle: avoid full viewport rebuilds on every token (large answers hang).
	lastPaint       time.Time
	paintPending    bool
	permissionMode  permissions.Mode
	mouseMode       string
	visionEnabled   bool
	visionSupported bool
	// followBottom is true when new content should keep the viewport pinned to
	// the latest line. Manual scrolling changes it until the user returns to
	// the bottom.
	followBottom     bool
	todosOpen        bool
	commandsOpen     bool
	themeName        string
	pendingApproval  *permissions.Request
	approvalDecision chan permissions.Decision
	tokenUsage       *llm.Usage
	// pendingRequests contains canonical, unrendered user input in FIFO order.
	pendingRequests []string
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

type modelDiscoveryMsg struct {
	generation uint64
	ids        []string
	err        error
}

func (m *model) relayout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	l := m.layoutFor(m.width, m.height)
	oldOffset := m.vp.YOffset
	m.vp.Width = l.conversationWidth
	m.vp.Height = l.conversationHeight
	if m.followBottom {
		m.vp.GotoBottom()
	} else {
		// Changing height can leave Bubble's offset past the new maximum.
		m.vp.SetYOffset(oldOffset)
	}
	m.ta.SetWidth(max(20, m.width-2))
	m.ta.SetHeight(l.inputHeight)
	m.renderer = newGlamourRenderer(max(40, m.messageWidth()), m.themeName)
}

func (m *model) syncLayout() bool {
	if m.width <= 0 || m.height <= 0 {
		return false
	}
	l := m.layoutFor(m.width, m.height)
	changed := m.vp.Width != l.conversationWidth || m.vp.Height != l.conversationHeight ||
		m.ta.Width() != max(20, m.width-2) || m.ta.Height() != l.inputHeight
	if changed {
		m.relayout()
	}
	return changed
}

func New(deps Deps) model {
	ta := textarea.New()
	ta.Placeholder = "Message…"
	ta.Focus()
	ta.Prompt = "› "
	// Bubbles renders a prompt for every physical and wrapped row by default.
	// Keep the Bolt gutter on the first row only; continuation rows retain the
	// prompt width without repeating the marker.
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "› "
		}
		return "  "
	})
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	applyTheme("dark")
	applyTextareaTheme(&ta, "dark")
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

	// Use an explicit style — WithAutoStyle() queries OSC 11 (bg color) and the
	// reply often appears as garbage in the input line on first launch. The
	// renderer is rebuilt on every theme switch in Update.
	r := newGlamourRenderer(80, "dark")

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
		followBottom:    true,
		themeName:       "dark",
		scrubLeft:       5, // a few startup passes to catch late OSC replies
		lines:           nil,
	}
	// Re-apply after the model value is constructed so textarea's internal style
	// pointer addresses this instance's FocusedStyle (see applyTextareaTheme).
	applyTextareaTheme(&m.ta, "dark")
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

func newGlamourRenderer(width int, theme string) *glamour.TermRenderer {
	if width < 20 {
		width = 80
	}
	style := glamourstyles.DarkStyleConfig
	codeTheme := "monokai"
	// Modest code-block surface only. Document/paragraph stay transparent so
	// assistant prose sits on the terminal background instead of a full-bleed band.
	codeBG := "236"
	if theme == "light" {
		style = glamourstyles.LightStyleConfig
		codeTheme = "github"
		codeBG = "254"
	}
	// Glamour registers its custom Chroma theme globally under one fixed name.
	// The first renderer (normally dark) therefore wins forever, even after a
	// theme switch. Use Chroma's built-in theme per Bolt theme instead; this
	// keeps syntax highlighting while making the code palette switchable.
	style.CodeBlock.Chroma = nil
	style.CodeBlock.Theme = codeTheme
	style.Document.StylePrimitive.BackgroundColor = nil
	style.Paragraph.StylePrimitive.BackgroundColor = nil
	style.CodeBlock.StylePrimitive.BackgroundColor = &codeBG
	// Glamour's standard styles reserve a two-cell document margin and another
	// margin around code blocks. Those are Markdown document-layout choices,
	// not source content, and make fenced code look padded or indented. Keep
	// Chroma and all semantic Markdown styling, but remove only those margins.
	style.Document.Margin = nil
	style.CodeBlock.Margin = nil
	// Prefer explicit styles over AutoStyle (no TTY color probes).
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
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
				if m.syncLayout() {
					m.refreshViewport()
				}
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
		m.relayout()
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
		if m.modelDiscoveryLoading {
			switch msg.String() {
			case "esc":
				m.cancelModelDiscovery()
				m.ta.Focus()
				m.status = "ready"
				m.lines = append(m.lines, chatLine{role: "system", text: "model discovery cancelled"})
				m.refreshViewport()
				return m, nil
			case "ctrl+c", "ctrl+q":
				m.cancelModelDiscovery()
				return m, tea.Quit
			default:
				// Keep discovery modal while resize and quit remain responsive.
				return m, nil
			}
		}
		if m.pendingApproval != nil && isApprovalDecisionKey(msg) {
			return m.handleApprovalKey(msg)
		}
		// Chat scroll keys — handle before the textarea so large answers are reachable.
		if m.handleChatScrollKey(msg) {
			m.followBottom = m.vp.AtBottom()
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
			m.relayout()
			m.refreshViewport()
			return m, nil
		case "ctrl+o":
			m.commandsOpen = !m.commandsOpen
			m.relayout()
			return m, nil
		case "ctrl+b":
			switch m.themeName {
			case "dark":
				m.themeName = "black"
			case "black":
				m.themeName = "light"
			default:
				m.themeName = "dark"
			}
			applyTheme(m.themeName)
			applyTextareaTheme(&m.ta, m.themeName)
			m.renderer = newGlamourRenderer(max(40, m.vp.Width-6), m.themeName)
			for i := range m.lines {
				m.lines[i].mdCache = ""
				m.lines[i].mdSrc = ""
			}
			m.refreshViewport()
			m.status = "theme: " + m.themeName
			return m, nil
		case "enter":
			text := strings.TrimSpace(m.ta.Value())
			if text == "" {
				return m, nil
			}
			m.ta.Reset()
			return m.handleSubmit(text)
		}
	case tea.MouseMsg:
		// Only interactive mouse mode consumes terminal mouse events. In SELECT
		// mode the terminal retains native selection behavior.
		if m.mouseMode == "INTERACTIVE" {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.followBottom = m.vp.AtBottom()
			return m, cmd
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

	case modelDiscoveryMsg:
		if msg.generation != m.modelDiscoverySeq || !m.modelDiscoveryLoading {
			return m, nil
		}
		m.modelDiscoveryLoading = false
		m.modelDiscoveryCancel = nil
		if msg.err != nil {
			m.ta.Focus()
			m.status = "ready"
			m.lines = append(m.lines, chatLine{
				role: "error",
				text: fmt.Sprintf("could not list models: %v\ncurrent: %s\nusage: /model <name>", msg.err, m.deps.Agent.Cfg.Model),
			})
			m.refreshViewport()
			return m, nil
		}
		if len(msg.ids) == 0 {
			m.ta.Focus()
			m.status = "ready"
			m.lines = append(m.lines, chatLine{
				role: "system",
				text: "no models reported by server\ncurrent: " + m.deps.Agent.Cfg.Model + "\nusage: /model <name>",
			})
			m.refreshViewport()
			return m, nil
		}
		idx := 0
		for i, id := range msg.ids {
			if id == m.deps.Agent.Cfg.Model {
				idx = i
				break
			}
		}
		m.modelPick = &modelPicker{ids: msg.ids, idx: idx}
		m.ta.Blur()
		m.status = "pick model · Tab next · Enter select · Esc cancel"
		m.refreshViewport()
		return m, nil

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
		if m.turnFailed {
			m.status = "error"
		} else {
			m.status = "ready"
		}
		m.events = nil
		m.cancel = nil
		// Auto-save rolling session after each turn (best-effort).
		if m.deps.SaveSession && m.deps.SessionPath != "" {
			_, _ = m.deps.Agent.SaveSessionPath(m.deps.SessionPath)
		}
		m.refreshViewport()
		if len(m.pendingRequests) > 0 {
			return m.startNextQueued()
		}
	}

	if m.modelPick == nil {
		oldViewportWidth, oldViewportHeight := m.vp.Width, m.vp.Height
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		cmds = append(cmds, cmd)
		if m.syncLayout() || m.vp.Width != oldViewportWidth || m.vp.Height != oldViewportHeight {
			m.refreshViewport()
		}
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
	if m.busy && !isStopCommand(text) {
		m.pendingRequests = append(m.pendingRequests, text)
		m.lines = append(m.lines, chatLine{role: "queued", text: text})
		m.status = fmt.Sprintf("queued · %d", len(m.pendingRequests))
		m.refreshViewport()
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		return m.handleSlash(text)
	}
	return m.startTurn(text)
}

func isStopCommand(text string) bool {
	parts := strings.Fields(text)
	return len(parts) == 1 && strings.EqualFold(parts[0], "/stop")
}

func (m model) startTurn(text string) (tea.Model, tea.Cmd) {
	m.lines = append(m.lines, chatLine{role: "user", text: text})
	m.ensureStream().Reset()
	m.turnAssistant = -1
	m.busy = true
	m.turnFailed = false
	m.gotToken = false
	m.waitSecs = 0
	m.busySince = time.Now()
	m.status = fmt.Sprintf("Thinking… (%s)", m.deps.Agent.Cfg.Model)
	// New turn: jump to the latest content so the question is visible.
	m.followBottom = true
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

func (m model) startNextQueued() (tea.Model, tea.Cmd) {
	if len(m.pendingRequests) == 0 {
		return m, nil
	}
	text := m.pendingRequests[0]
	m.pendingRequests = m.pendingRequests[1:]
	for i := range m.lines {
		if m.lines[i].role == "queued" && m.lines[i].text == text {
			m.lines = append(m.lines[:i], m.lines[i+1:]...)
			break
		}
	}
	return m.handleSubmit(text)
}

func isApprovalDecisionKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "1", "2", "3", "4", "y", "n", "enter", "esc":
		return true
	default:
		return false
	}
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
		m.relayout()
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
		m.relayout()
		line := formatToolEnd(ev.Tool, ev.ToolOut, m.verbose)
		toolFailed := strings.HasPrefix(strings.TrimSpace(ev.ToolOut), "error:") && !isBenignToolFailure(ev.Tool, ev.ToolOut)
		if m.verbose {
			m.lines = append(m.lines, chatLine{role: "tool", text: line})
		}
		if !m.verbose && toolFailed {
			m.lines = append(m.lines, chatLine{role: "error", text: line})
		}
		if toolFailed {
			m.turnFailed = true
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
		if !isCancellationText(ev.Text) {
			m.turnFailed = true
		}
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
		if m.turnFailed {
			m.status = "error"
		} else {
			m.status = "finalizing…"
		}
		return m, m.schedulePaint(true)
	}
	return m, nil
}

func isCancellationText(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "cancelled", "canceled":
		return true
	default:
		return false
	}
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
  Shift+Enter     Newline in prompt
  Esc / /stop     Cancel in-flight reply
  Ctrl+C          Cancel if busy; quit when idle
  Ctrl+L          Toggle mouse mode
  Ctrl+Y          Toggle vision state
  Ctrl+T          Toggle todos
  Ctrl+O          Show commands
  Ctrl+B          Toggle theme
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

func (m *model) cancelModelDiscovery() {
	if m.modelDiscoveryCancel != nil {
		m.modelDiscoveryCancel()
	}
	m.modelDiscoveryCancel = nil
	m.modelDiscoveryLoading = false
	m.modelDiscoverySeq++
}

func discoverModelsCmd(client *llm.Client, ctx context.Context, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ids, err := client.ListModels(ctx)
		return modelDiscoveryMsg{generation: generation, ids: ids, err: err}
	}
}

// handleModelCmd implements Grok-style mid-chat model switch and listing.
// /model or /model list opens an interactive picker: Tab cycles, Enter selects, Esc cancels.
// /model <name> still switches immediately.
func (m model) handleModelCmd(parts []string) (model, tea.Cmd) {
	// /model  or  /model list  → asynchronously load the interactive picker.
	if len(parts) < 2 || strings.EqualFold(parts[1], "list") || strings.EqualFold(parts[1], "ls") {
		if m.modelDiscoveryLoading {
			return m, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.modelDiscoverySeq++
		generation := m.modelDiscoverySeq
		m.modelDiscoveryLoading = true
		m.modelDiscoveryCancel = cancel
		m.modelPick = nil
		m.ta.Blur()
		m.status = "loading models… (Esc cancel)"
		m.refreshViewport()
		return m, discoverModelsCmd(m.deps.Agent.Client, ctx, generation)
	}

	// A direct model selection supersedes any in-flight discovery. Normal key
	// input is modal while loading, but this also protects programmatic callers.
	if m.modelDiscoveryLoading {
		m.cancelModelDiscovery()
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

// chatBubble adds message-content padding without painting a full-width
// background. The header is styled separately by refreshViewport, while body
// renderers retain ownership of Markdown/code styling.
func chatBubble(base lipgloss.Style, label, body string, width int) string {
	if width < 10 {
		width = 10
	}
	// Keep the size argument for the existing call sites and large-message
	// policy. Width must not be combined with a background-bearing style here.
	if len(body) > bubbleMaxBytes {
		return base.Render(label) + "\n" + body
	}
	return base.Render(label + "\n" + body)
}

func (m *model) renderAssistantBody(ln *chatLine, width int) string {
	if m.renderer == nil || ln.text == "" {
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
		if hasFencedCodeBlock(ln.text) {
			// Glamour's block writer pads every rendered line to the document
			// width. Trim only that renderer padding for fenced-code responses;
			// do not normalize newlines or prose whitespace. Then tint the
			// content-sized lines with a modest code surface (not full-bleed).
			out = tintCodeBlockSurface(trimANSIHorizontalPadding(out), m.themeName)
		}
		ln.mdCache = out
		ln.mdSrc = ln.text
		return out
	}
	return wrap(ln.text, width-4)
}

// tintCodeBlockSurface applies a restrained theme-aware background to each
// visible code line without stretching short blocks to the conversation width.
func tintCodeBlockSurface(s, theme string) string {
	bg := lipgloss.Color("#1e293b")
	if theme == "light" {
		bg = lipgloss.Color("#f1f5f9")
	} else if theme == "black" {
		bg = lipgloss.Color("#0f172a")
	}
	tint := lipgloss.NewStyle().Background(bg)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.TrimSpace(ansiCSI.ReplaceAllString(line, "")) == "" {
			continue
		}
		lines[i] = tint.Render(line)
	}
	return strings.Join(lines, "\n")
}

func hasFencedCodeBlock(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return true
		}
	}
	return false
}

// trimANSIHorizontalPadding removes visible trailing spaces while preserving
// ANSI style sequences. Glamour's padding writer emits those spaces in styled
// chunks, so strings.TrimRight alone cannot remove them.
func trimANSIHorizontalPadding(s string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		body := strings.TrimSuffix(line, "\n")
		type token struct {
			raw   string
			style bool
			space bool
		}
		var tokens []token
		for len(body) > 0 {
			if loc := ansiCSI.FindStringIndex(body); loc != nil && loc[0] == 0 {
				tokens = append(tokens, token{raw: body[:loc[1]], style: true})
				body = body[loc[1]:]
				continue
			}
			r, size := utf8.DecodeRuneInString(body)
			tokens = append(tokens, token{raw: body[:size], space: unicode.IsSpace(r)})
			body = body[size:]
		}
		lastVisible := -1
		for i, tok := range tokens {
			if !tok.style && !tok.space {
				lastVisible = i
			}
		}
		for i, tok := range tokens {
			if i <= lastVisible || tok.style {
				out.WriteString(tok.raw)
			}
		}
		if strings.HasSuffix(line, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func (m *model) refreshViewport() {
	// Ensure content is painted with the final viewport dimensions. This also
	// makes refreshes triggered by optional panels use the same geometry as
	// View, before bottom state is evaluated.
	m.syncLayout()
	// Reconcile direct viewport movement as well as movements handled by our
	// key/mouse paths. This is important during resize and keeps tests or other
	// callers that adjust YOffset from being unexpectedly pulled to the bottom.
	if m.followBottom && !m.vp.AtBottom() {
		m.followBottom = false
	}
	var b strings.Builder
	col := m.vp.Width
	if col < 20 {
		col = 80
	}
	width := contentMaxWidth(col)
	for i := range m.lines {
		ln := &m.lines[i]
		switch ln.role {
		case "user":
			// Foreground-only labels: no filled message cards.
			label := styleUser.Render("You")
			body := wrap(ln.text, width)
			b.WriteString(chatBubble(styleUserBubble, label, body, width) + "\n\n")
		case "assistant", "assistant-stream":
			labelText := "Agent"
			if ln.role == "assistant-stream" {
				labelText = "Agent …"
			}
			label := styleAsst.Render(labelText)
			rendered := m.renderAssistantBody(ln, width)
			b.WriteString(chatBubble(styleAsstBubble, label, rendered, width) + "\n\n")
		case "thinking":
			label := styleAsst.Render("Agent")
			body := styleStatus.Render(wrap(ln.text, width))
			b.WriteString(chatBubble(styleAsstBubble, label, body, width) + "\n\n")
		case "tool":
			b.WriteString(styleTool.Render("Tool") + "\n")
			b.WriteString(styleTool.Render(wrap(ln.text, width)) + "\n\n")
		case "queued":
			queued := len(m.pendingRequests)
			if queued < 1 {
				queued = 1
			}
			b.WriteString(styleStatus.Render(fmt.Sprintf("Queued · %d", queued)) + "\n")
			b.WriteString(styleStatus.Render(wrap(ln.text, width)) + "\n\n")
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
	m.vp.SetContent(b.String())
	if m.followBottom {
		m.vp.GotoBottom()
	} else {
		m.vp.SetYOffset(m.vp.YOffset)
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
	case msg.String() == "up" && strings.TrimSpace(m.ta.Value()) == "":
		m.vp.ScrollUp(1)
		return true
	case msg.String() == "down" && strings.TrimSpace(m.ta.Value()) == "":
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
	m.relayout()
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
		return "Todos\nNo todos"
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

func renderTodoPanel(width, height int) string {
	// Subtle secondary pane: normal border, no heavy fill. Size content so the
	// outer panel occupies exactly `width` and aligns with the conversation column.
	inner := max(10, width-styleTodo.GetHorizontalFrameSize())
	// Height excludes the border rows so the outer panel matches viewport height.
	return styleTodo.Width(inner).Height(max(1, height-2)).Render(todoText())
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
		colorMuted = lipgloss.Color("#cbd5e1")
		colorAccent = lipgloss.Color("#67e8f9")
		colorUser = lipgloss.Color("#c4b5fd")
		colorAsst = lipgloss.Color("#86efac")
		colorTool = lipgloss.Color("#fde68a")
		colorError = lipgloss.Color("#fca5a5")
		colorBorder = lipgloss.Color("#334155")
		colorBackground = lipgloss.Color("#020617")
		colorForeground = lipgloss.Color("#f8fafc")
		bgUser = lipgloss.AdaptiveColor{Light: "#dbeafe", Dark: "#111827"}
		bgAsst = lipgloss.AdaptiveColor{Light: "#f8fafc", Dark: "#020617"}
		fgBody = lipgloss.AdaptiveColor{Light: "#0f172a", Dark: "#f8fafc"}
	} else if name == "light" {
		colorMuted = lipgloss.Color("#475569")
		colorAccent = lipgloss.Color("#0369a1")
		colorUser = lipgloss.Color("#6d28d9")
		colorAsst = lipgloss.Color("#15803d")
		colorTool = lipgloss.Color("#a16207")
		colorError = lipgloss.Color("#b91c1c")
		colorBorder = lipgloss.Color("#cbd5e1")
		colorBackground = lipgloss.Color("#ffffff")
		colorForeground = lipgloss.Color("#0f172a")
		// Concrete colors (both Adaptive slots identical) so Light never depends
		// on terminal background detection when painting message headers.
		bgUser = lipgloss.AdaptiveColor{Light: "#e0e7ff", Dark: "#e0e7ff"}
		bgAsst = lipgloss.AdaptiveColor{Light: "#f8fafc", Dark: "#f8fafc"}
		fgBody = lipgloss.AdaptiveColor{Light: "#0f172a", Dark: "#0f172a"}
	} else {
		colorMuted = lipgloss.Color("#94a3b8")
		colorAccent = lipgloss.Color("#38bdf8")
		colorUser = lipgloss.Color("#a78bfa")
		colorAsst = lipgloss.Color("#4ade80")
		colorTool = lipgloss.Color("#fbbf24")
		colorError = lipgloss.Color("#f87171")
		colorBorder = lipgloss.Color("#1e293b")
		colorBackground = lipgloss.Color("#0f172a")
		colorForeground = lipgloss.Color("#e2e8f0")
		bgUser = lipgloss.AdaptiveColor{Light: "#e5e7eb", Dark: "#1e293b"}
		bgAsst = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0f172a"}
		fgBody = lipgloss.AdaptiveColor{Light: "#1e293b", Dark: "#e2e8f0"}
	}
	styleHeader = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleStatus = lipgloss.NewStyle().Foreground(colorMuted)
	styleHelp = lipgloss.NewStyle().Foreground(colorMuted)
	styleUser = lipgloss.NewStyle().Foreground(colorUser).Bold(true)
	styleAsst = lipgloss.NewStyle().Foreground(colorAsst).Bold(true)
	styleTool = lipgloss.NewStyle().Foreground(colorTool)
	styleErr = lipgloss.NewStyle().Foreground(colorError)
	styleBox = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(colorBorder).Padding(0, 1)
	styleConversation = lipgloss.NewStyle().Foreground(fgBody)
	styleUserBubble = lipgloss.NewStyle().Foreground(fgBody)
	styleAsstBubble = lipgloss.NewStyle().Foreground(fgBody)
	styleTodo = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(colorBorder).Foreground(fgBody).Padding(0, 1)
	styleRoot = lipgloss.NewStyle().Foreground(colorForeground)
}

func applyTextareaTheme(ta *textarea.Model, name string) {
	if ta == nil {
		return
	}
	// Prompt-like input: theme foregrounds without a painted panel. Bubbles still
	// needs a background on faces that pad with spaces, so use the theme canvas
	// color only as a quiet match for the terminal — not a visible box.
	fg := colorForeground
	bg := colorBackground
	switch name {
	case "black":
		fg = lipgloss.Color("#f8fafc")
		bg = lipgloss.Color("#020617")
	case "light":
		fg = lipgloss.Color("#0f172a")
		bg = lipgloss.Color("#ffffff")
	default:
		fg = lipgloss.Color("#e2e8f0")
		bg = lipgloss.Color("#0f172a")
	}
	base := lipgloss.NewStyle().Foreground(fg).Background(bg)
	muted := lipgloss.NewStyle().Foreground(colorMuted).Background(bg)
	prompt := lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Background(bg)
	cursor := lipgloss.NewStyle().Foreground(colorAccent).Background(bg)
	for _, style := range []*textarea.Style{&ta.FocusedStyle, &ta.BlurredStyle} {
		style.Base = base
		style.CursorLine = lipgloss.NewStyle().Background(bg)
		style.CursorLineNumber = lipgloss.NewStyle().Foreground(colorMuted).Background(bg)
		style.EndOfBuffer = muted
		style.LineNumber = muted
		style.Placeholder = muted
		style.Prompt = prompt
		style.Text = base
	}
	ta.Cursor.Style = cursor
	ta.Cursor.TextStyle = base
	// Model/textarea are copied by value in New and Bubble Tea updates. Focus()
	// stores &FocusedStyle in an unexported pointer; after a copy that pointer
	// still references the previous value's styles, so View() would keep painting
	// the old theme. Re-bind it to this textarea instance.
	if ta.Focused() {
		_ = ta.Focus()
	} else {
		ta.Blur()
	}
}

// paintRow renders a single chrome row that occupies the full allocated width.
// Lip Gloss JoinVertical/JoinHorizontal pad short lines with unstyled spaces;
// those unpainted cells show the terminal default background (often black) in
// Light theme. Pad with an explicit background instead of Style.Width, which
// would wrap a long status line onto a second row.
func paintRow(style lipgloss.Style, width int, content string) string {
	rendered := style.Render(content)
	if width < 1 {
		return rendered
	}
	if gap := width - lipgloss.Width(rendered); gap > 0 {
		rendered += lipgloss.NewStyle().Background(style.GetBackground()).Render(strings.Repeat(" ", gap))
	}
	return rendered
}

// paintSurface trims viewport/textarea trailing space pads (which are unstyled)
// and re-paints every line to width with the active canvas background without
// wrapping existing content.
func paintSurface(style lipgloss.Style, width, height int, view string) string {
	if width < 1 {
		width = 1
	}
	lines := strings.Split(trimANSIHorizontalPadding(view), "\n")
	if height > 0 {
		for len(lines) < height {
			lines = append(lines, "")
		}
		if len(lines) > height {
			lines = lines[:height]
		}
	}
	pad := lipgloss.NewStyle().Foreground(style.GetForeground()).Background(style.GetBackground())
	for i, line := range lines {
		gap := width - lipgloss.Width(line)
		if gap <= 0 {
			continue
		}
		if line == "" {
			lines[i] = pad.Render(strings.Repeat(" ", gap))
			continue
		}
		lines[i] = line + pad.Render(strings.Repeat(" ", gap))
	}
	return strings.Join(lines, "\n")
}

// joinHorizontalThemed joins columns like lipgloss.JoinHorizontal but pads
// short lines with the theme background instead of bare spaces.
func joinHorizontalThemed(bg lipgloss.TerminalColor, parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	pad := lipgloss.NewStyle().Background(bg)
	blocks := make([][]string, len(parts))
	maxWidths := make([]int, len(parts))
	maxHeight := 0
	for i, str := range parts {
		lines := strings.Split(str, "\n")
		blocks[i] = lines
		for _, line := range lines {
			if w := lipgloss.Width(line); w > maxWidths[i] {
				maxWidths[i] = w
			}
		}
		if len(lines) > maxHeight {
			maxHeight = len(lines)
		}
	}
	for i := range blocks {
		for len(blocks[i]) < maxHeight {
			blocks[i] = append(blocks[i], "")
		}
	}
	var b strings.Builder
	for row := 0; row < maxHeight; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		for i, block := range blocks {
			line := block[row]
			b.WriteString(line)
			if gap := maxWidths[i] - lipgloss.Width(line); gap > 0 {
				b.WriteString(pad.Render(strings.Repeat(" ", gap)))
			}
		}
	}
	return b.String()
}

func padBlock(s string, width, height int) string {
	if width < 1 {
		width = 1
	}
	lines := strings.Split(s, "\n")
	if height > 0 {
		for len(lines) < height {
			lines = append(lines, "")
		}
		if len(lines) > height {
			lines = lines[:height]
		}
	}
	for i, line := range lines {
		if gap := width - lipgloss.Width(line); gap > 0 {
			lines[i] = line + strings.Repeat(" ", gap)
		}
	}
	return strings.Join(lines, "\n")
}

// fitFrameHeight makes View's output an exact terminal-sized frame. The fixed
// regions are composed last, so if an unusually small terminal cannot display
// every optional panel, retaining the bottom rows is the least surprising and
// prevents a previous frame's rows from being mistaken for current content.
func fitFrameHeight(s string, height int) string {
	if height <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m model) View() string {
	l := m.layoutFor(m.width, m.height)
	w := l.width
	// The viewport and Todo panel are the only children of the scrolling-region
	// row. Both receive dimensions from the same layout calculation, so the
	// panel cannot drift into the fixed status/input/footer rows.
	body := trimANSIHorizontalPadding(m.vp.View())
	body = padBlock(body, l.conversationWidth, l.conversationHeight)
	if l.todoWidth > 0 {
		todo := renderTodoPanel(l.todoWidth, l.conversationHeight)
		body = joinHorizontalThemed(lipgloss.NoColor{}, body, todo)
	}
	status := m.status
	if m.busy && (status == "ready" || status == "") {
		status = "streaming"
	}
	if status != "" {
		status = strings.ToUpper(status[:1]) + status[1:]
	}
	modelName := m.deps.Agent.Cfg.Model
	if modelName == "" {
		modelName = m.deps.Agent.Cfg.Provider
	}
	tokens := "—"
	if m.tokenUsage != nil {
		total := m.tokenUsage.TotalTokens
		if total == 0 {
			total = m.tokenUsage.PromptTokens + m.tokenUsage.CompletionTokens
		}
		if total > 0 {
			tokens = fmt.Sprintf("%d tokens", total)
		}
	}
	vision := "Vision OFF"
	if m.visionEnabled {
		vision = "Vision ON"
	}
	// One subtle status line — no filled bar.
	statusText := fmt.Sprintf("Bolt · %s · %s · %s · %s · %s · %s",
		strings.ToUpper(string(m.permissionMode)),
		m.mouseMode,
		vision,
		modelName,
		tokens,
		status,
	)
	statusLine := styleStatus.Render(truncate(statusText, w))
	helpText := "Ctrl+Q quit · Ctrl+R permission · Ctrl+L mouse · Ctrl+Y vision · Ctrl+T todos · Ctrl+O commands · Ctrl+B theme · Enter send · Shift+Enter newline"
	if m.deps.Agent != nil && m.deps.Agent.PlanMode {
		helpText = "PLAN MODE · Ctrl+Q quit · Ctrl+R permission · Enter send · Shift+Enter newline"
	}
	if m.modelPick != nil {
		helpText = "Tab / ↓ next · Shift+Tab / ↑ prev · Enter select · Esc cancel · 1-9 quick"
	}
	help := styleHelp.Render(truncate(helpText, w))
	workspace := m.deps.Workspace
	if workspace == "" {
		workspace = mustCwd()
	}
	cwdLine := styleHelp.Render(truncate("📁 "+displayCwdAt(workspace, max(20, w-4)), w))
	// Prompt-like input: no rounded/boxed frame.
	input := trimANSIHorizontalPadding(m.ta.View())
	parts := []string{styleHeader.Render(truncate(m.deps.Title+" · "+m.deps.Summary+" · /help", w)), body}
	parts = append(parts, statusLine)
	dialogW := max(10, w)
	if m.pendingApproval != nil {
		parts = append(parts, styleBox.Width(dialogW).Render(approvalText(m.pendingApproval)))
	}
	if m.commandsOpen {
		parts = append(parts, styleBox.Width(dialogW).Render(commandsText()))
	}
	if m.modelPick != nil {
		parts = append(parts, m.modelPickerView(dialogW+2))
	}
	parts = append(parts, cwdLine, input, help)
	return fitFrameHeight(lipgloss.JoinVertical(lipgloss.Left, parts...), l.height)
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

func min(a, b int) int {
	if a < b {
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
