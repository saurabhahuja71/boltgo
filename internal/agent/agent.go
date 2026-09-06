package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// Event kinds streamed to the TUI.
type EventKind int

const (
	EventToken EventKind = iota
	EventToolStart
	EventToolEnd
	EventError
	EventDone
	EventStatus
	EventPermission
	EventUsage
)

type Event struct {
	Kind       EventKind
	Text       string
	Tool       string
	ToolOut    string
	Permission *permissions.Request
	Decision   chan permissions.Decision
	Usage      *llm.Usage
}

// Agent runs the multi-turn tool loop against an OpenAI-compatible model.
type Agent struct {
	Cfg    config.Config
	Client *llm.Client
	Tools  *tools.Registry
	// History is the full conversation (including system).
	History []llm.Message
	// MaxToolRounds prevents infinite tool loops.
	MaxToolRounds int
	// PlanMode: Grok-like plan first — no tools; model only outlines steps.
	PlanMode    bool
	Permissions *permissions.Manager
}

func New(cfg config.Config, client *llm.Client, reg *tools.Registry) *Agent {
	e := cfg.Effective()
	hist := []llm.Message{}
	sys := strings.TrimSpace(e.SystemPrompt)
	extra := workspaceHint(e.Workspace)
	if rules := loadProjectRules(e.Workspace); rules != "" {
		extra = extra + "\n\n" + rules
	}
	if sys != "" {
		sys = sys + "\n\n" + extra
		hist = append(hist, llm.Message{Role: llm.RoleSystem, Content: sys})
	} else {
		hist = append(hist, llm.Message{Role: llm.RoleSystem, Content: extra})
	}
	return &Agent{
		Cfg:           e,
		Client:        client,
		Tools:         reg,
		History:       hist,
		MaxToolRounds: 8,
		Permissions:   permissions.New(permissions.Mode(e.PermissionMode), permissions.DefaultStorePath()),
	}
}

// workspaceHint tells the model where tools resolve paths (critical for repo reads).
func workspaceHint(workspace string) string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = "."
	}
	if workspace != "" {
		cwd = workspace
	}
	root := findRepoRootAt(cwd)
	return strings.TrimSpace(fmt.Sprintf(`
Workspace (tool paths resolve here):
- Current working directory: %s
- Detected project root: %s
- Paths for read_file / list_dir / write_file / str_replace / find_files / grep / git / run_tests are relative to cwd (or absolute).
- User can attach context with @path (e.g. @README.md @internal/agent).
- Do NOT invent prefixes like "repo/" or invent file names (no fake main.go/config.go lists).
- Only report paths that appeared in tool results or @mentions.
- Prefer grep / repo_map to explore; find_files to locate names; str_replace to edit; run_tests after code changes.
- Link checks: grep for https?:// in files, then fetch each URL (cap ~15). Never xargs+curl/wget crawls.
- For "can you do it" / apply / implement: use tools. Do not only print a plan.
`, cwd, root))
}

// Reset clears chat history but keeps system prompt.
func (a *Agent) Reset() {
	sys := ""
	if len(a.History) > 0 && a.History[0].Role == llm.RoleSystem {
		sys = a.History[0].Content
	}
	a.History = nil
	if sys != "" {
		a.History = []llm.Message{{Role: llm.RoleSystem, Content: sys}}
	}
}

// LastUserText is the last user-visible prompt (for /retry).
func (a *Agent) LastUserText() string {
	for i := len(a.History) - 1; i >= 0; i-- {
		if a.History[i].Role == llm.RoleUser {
			// strip agenterm injects
			s := a.History[i].Content
			if j := strings.Index(s, "\n\n[agenterm]"); j >= 0 {
				s = s[:j]
			}
			if j := strings.Index(s, "\n\n---\nAttached context"); j >= 0 {
				s = s[:j]
			}
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// PopLastExchange removes the last user message and everything after it (for /retry).
func (a *Agent) PopLastExchange() string {
	user := a.LastUserText()
	for i := len(a.History) - 1; i >= 0; i-- {
		if a.History[i].Role == llm.RoleUser {
			a.History = a.History[:i]
			return user
		}
	}
	return ""
}

// CompactHistory drops old tool payloads if history is large (keeps recent turns).
func (a *Agent) CompactHistory() {
	const softLimit = 80_000
	total := 0
	for _, m := range a.History {
		total += len(m.Content)
	}
	if total < softLimit {
		return
	}
	// Shrink older tool messages
	for i := 0; i < len(a.History)-6; i++ {
		if a.History[i].Role == llm.RoleTool && len(a.History[i].Content) > 500 {
			a.History[i].Content = a.History[i].Content[:500] + "\n…[compacted]…"
		}
	}
}

// RunUserMessage appends a user message and runs the agent loop, emitting events.
func (a *Agent) RunUserMessage(ctx context.Context, user string, emit func(Event)) error {
	if a.Permissions == nil {
		a.Permissions = permissions.New(permissions.ModeAllow, permissions.DefaultStorePath())
	}
	a.CompactHistory()

	// @path mentions → attach file/dir context
	payload, attached := expandMentions(user)
	if attached != "" {
		emit(Event{Kind: EventStatus, Text: "attached @" + attached})
	}

	// Action requests: nudge the model in the same user turn so it executes tools.
	if isActionRequest(user) {
		payload = payload + "\n\n[agenterm] Execute now with tools (str_replace/write_file/git/grep/run_tests). Do not only print steps."
		emit(Event{Kind: EventStatus, Text: "action mode: will apply changes via tools"})
	}
	if isLinkCheckRequest(user) && !a.PlanMode {
		payload = payload + "\n\n[agenterm] LINK CHECK: Do NOT print shell commands. " +
			"1) Call the grep tool with pattern https?:// (or http) on the repo. " +
			"2) Call fetch for each unique http(s) URL found (max 15). " +
			"3) Report only broken/non-200 links. Never use xargs, pipelines, or run_shell for this."
		emit(Event{Kind: EventStatus, Text: "link-check mode: built-in grep + fetch"})
	}
	if a.PlanMode {
		payload = payload + "\n\n[agenterm] PLAN MODE: Do not call tools. Produce a numbered plan only " +
			"(goal, steps, files to touch, risks). Wait for the user to say /plan off and implement."
		emit(Event{Kind: EventStatus, Text: "plan mode: tools off — outline steps only"})
	}
	a.History = append(a.History, llm.Message{Role: llm.RoleUser, Content: payload})

	// Deterministic link check: models dump shell and leave an empty "ready" UI.
	// Run grep+fetch ourselves and always emit a real report.
	if isLinkCheckRequest(user) && !a.PlanMode && a.Cfg.EnableTools && a.Tools != nil {
		emit(Event{Kind: EventStatus, Text: "running built-in link check…"})
		report := a.runDeterministicLinkCheck(ctx, emit)
		if report == "cancelled" {
			emit(Event{Kind: EventError, Text: "cancelled"})
			emit(Event{Kind: EventDone})
			return ctx.Err()
		}
		a.History = append(a.History, llm.Message{Role: llm.RoleAssistant, Content: report})
		emit(Event{Kind: EventToken, Text: report})
		emit(Event{Kind: EventDone})
		return nil
	}

	// Attach tools only when enabled and the turn is not pure small-talk.
	// Skipping tools for greetings avoids a pointless second LLM round-trip
	// (common with Ollama models that eagerly call list_dir on "hi").
	var toolSchemas []llm.Tool
	attachTools := a.Cfg.EnableTools && a.Tools != nil && !isTrivialChat(user) && !a.PlanMode
	// @mentions or action → always allow tools (unless plan mode)
	if a.Cfg.EnableTools && a.Tools != nil && !a.PlanMode && (attached != "" || isActionRequest(user)) {
		attachTools = true
	}
	if attachTools {
		toolSchemas = a.Tools.LLMTools()
	} else if a.Cfg.EnableTools && a.Tools != nil && isTrivialChat(user) {
		emit(Event{Kind: EventStatus, Text: "tools skipped (chat-only turn)"})
	}

	maxRounds := a.MaxToolRounds
	if isActionRequest(user) && maxRounds < 12 {
		maxRounds = 12 // branch + edit + commit may need more steps
	}

	toolsUsed := 0
	for round := 0; round < maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			emit(Event{Kind: EventError, Text: "cancelled"})
			emit(Event{Kind: EventDone})
			return err
		}

		// Build request messages; after tools, add a non-persisted brief-answer nudge.
		msgs := a.History
		if toolsUsed > 0 {
			msgs = append(append([]llm.Message{}, a.History...), llm.Message{
				Role:    llm.RoleUser,
				Content: afterToolsAnswerHint(user, toolsUsed),
			})
		} else if isActionRequest(user) && round == 0 {
			msgs = append(append([]llm.Message{}, a.History...), llm.Message{
				Role: llm.RoleUser,
				Content: `The user wants real on-disk changes. Call tools now:
1) read_file if needed, 2) str_replace or write_file, 3) git add/commit/push only if they asked.
Do not answer with only a markdown plan or shell snippets.`,
			})
		}

		req := llm.ChatRequest{
			Model:       a.Cfg.Model,
			Messages:    msgs,
			Temperature: a.Cfg.Temperature,
			MaxTokens:   a.Cfg.MaxTokens,
		}
		// After tools: cooler sampling + shorter completion → less rambling / fake lists.
		if toolsUsed > 0 {
			if req.Temperature <= 0 || req.Temperature > 0.3 {
				req.Temperature = 0.2
			}
			if req.MaxTokens == 0 || req.MaxTokens > 600 {
				req.MaxTokens = 600
			}
		}

		// Multi-step tools allowed; action tasks get more tool rounds before we force an answer.
		toolCap := 4
		if isActionRequest(user) {
			toolCap = 10
		}
		roundTools := toolSchemas
		if round > 0 && a.Cfg.EnableTools && a.Tools != nil && toolsUsed < toolCap && round < toolCap {
			roundTools = a.Tools.LLMTools()
		}
		if toolsUsed >= toolCap || round >= toolCap {
			roundTools = nil // force plain-text answer
			emit(Event{Kind: EventStatus, Text: "final answer (no more tools)"})
		}
		if len(roundTools) > 0 {
			req.Tools = roundTools
			req.ToolChoice = "auto"
			if isActionRequest(user) && toolsUsed == 0 && round == 0 {
				// Encourage tool use on first action turn (OpenAI-compatible; Ollama may ignore).
				req.ToolChoice = "auto"
			}
		} else if !attachTools && isTrivialChat(user) {
			req.ToolChoice = "none"
		}

		emit(Event{Kind: EventStatus, Text: fmt.Sprintf("calling %s (round %d)…", a.Cfg.Model, round+1)})
		handler := &streamBridge{emit: emit}
		msg, err := a.Client.ChatStream(ctx, req, handler)
		if err != nil {
			if ctx.Err() != nil {
				emit(Event{Kind: EventError, Text: "cancelled"})
			} else {
				emit(Event{Kind: EventError, Text: err.Error()})
			}
			emit(Event{Kind: EventDone})
			return err
		}
		if msg.Usage != nil {
			usage := *msg.Usage
			emit(Event{Kind: EventUsage, Usage: &usage})
		}

		// Ollama/Qwen often print tools as plain JSON content — recover and run them.
		if len(msg.ToolCalls) == 0 && len(roundTools) > 0 && a.Tools != nil {
			if recovered, rest := extractToolCallsFromContent(msg.Content, toolNameSet(a.Tools)); len(recovered) > 0 {
				emit(Event{Kind: EventStatus, Text: fmt.Sprintf("recovered %d tool call(s) from text", len(recovered))})
				msg.ToolCalls = recovered
				msg.Content = rest
			}
		}
		// Models dump bare shell (grep|xargs…) instead of tool_calls — recover or refuse.
		if len(msg.ToolCalls) == 0 && len(roundTools) > 0 && a.Tools != nil {
			if recovered, rest, note := recoverShellishContent(msg.Content, toolNameSet(a.Tools)); len(recovered) > 0 {
				emit(Event{Kind: EventStatus, Text: note})
				msg.ToolCalls = recovered
				msg.Content = rest
			} else if note != "" {
				emit(Event{Kind: EventStatus, Text: note})
				// Replace shell dump with a short redirect so TUI does not show it as the answer.
				if isShellOnlyAssistantText(msg.Content) {
					msg.Content = ""
				}
			}
		}

		// Persist assistant turn (without the ephemeral after-tools system nudge)
		a.History = append(a.History, msg)

		if len(msg.ToolCalls) == 0 {
			// Shell dump / empty — don't leave the user staring at a blank "ready".
			trim := strings.TrimSpace(msg.Content)
			if trim == "" || isShellOnlyAssistantText(trim) {
				// Fix history: last assistant was empty/shell — replace content with a short note
				// so the next API round is not confused (do not append a second assistant).
				if n := len(a.History); n > 0 && a.History[n-1].Role == llm.RoleAssistant {
					a.History[n-1].Content = "(ignored shell/empty; use tools next)"
				}
				if toolsUsed == 0 && round == 0 && attachTools {
					// Force one more round with an explicit system nudge (not a fake tool_call pair).
					emit(Event{Kind: EventStatus, Text: "empty/shell reply — asking model again with tools"})
					// Drop the useless assistant turn so we don't poison history.
					if n := len(a.History); n > 0 && a.History[n-1].Role == llm.RoleAssistant {
						a.History = a.History[:n-1]
					}
					// Fall through by continuing loop without toolsUsed bump — same user, retry round.
					// Inject ephemeral nudge via toolsUsed path: set toolsUsed=-1 trick? Use a flag.
					// Simpler: append a system-visible user hint once.
					a.History = append(a.History, llm.Message{
						Role:    llm.RoleUser,
						Content: "[agenterm] Your previous reply was empty or only a shell command. Call real tools now (grep/read_file/fetch). Do not print shell.",
					})
					continue
				}
				emit(Event{Kind: EventToken, Text: "(no answer — model returned empty or a shell command. Try /retry with a clearer request, or /tools on.)"})
			}
			emit(Event{Kind: EventDone})
			return nil
		}

		// Sanitize tool_calls before history so Ollama doesn't 400 on next round.
		for i := range msg.ToolCalls {
			msg.ToolCalls[i].Function.Arguments = sanitizeToolArgsJSON(msg.ToolCalls[i].Function.Name, msg.ToolCalls[i].Function.Arguments)
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", round, i)
			}
			if msg.ToolCalls[i].Type == "" {
				msg.ToolCalls[i].Type = "function"
			}
		}
		// Re-bind last history assistant message if we already appended (we append before tools)
		// Fix: we appended msg above — update it with sanitized args
		if n := len(a.History); n > 0 {
			a.History[n-1] = msg
		}

		// Execute tools sequentially (each tool has its own timeout inside the runner).
		for _, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				emit(Event{Kind: EventError, Text: "cancelled"})
				emit(Event{Kind: EventDone})
				return err
			}
			name := tc.Function.Name
			args := tc.Function.Arguments
			level, capability := permissions.LevelForTool(name, args)
			request := permissions.Request{Tool: name, Capability: capability, Level: level, Arguments: args}
			decision, prompt := a.Permissions.Check(request)
			if prompt {
				response := make(chan permissions.Decision, 1)
				emit(Event{Kind: EventPermission, Tool: name, Text: args, Permission: &request, Decision: response})
				select {
				case decision = <-response:
				case <-ctx.Done():
					decision = permissions.Deny
				}
				decision = a.Permissions.Commit(request, decision)
			}
			emit(Event{Kind: EventToolStart, Tool: name, Text: args})
			emit(Event{Kind: EventStatus, Text: fmt.Sprintf("running %s…", name)})
			var out string
			var err error
			if decision == permissions.Deny {
				out = "error: permission denied"
			} else {
				out, err = a.Tools.Run(ctx, name, args)
			}
			if err != nil {
				out = fmt.Sprintf("error: %v\n%s", err, out)
			}
			// Cap what the model sees so it does not re-dump huge listings into chat.
			outForModel := capToolResult(out, 6_000)
			emit(Event{Kind: EventToolEnd, Tool: name, ToolOut: out}) // TUI uses compact formatter
			a.History = append(a.History, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Content:    outForModel,
				Name:       name,
			})
			toolsUsed++
		}
		// loop: model continues with tool results
	}
	emit(Event{Kind: EventError, Text: "max tool rounds reached"})
	emit(Event{Kind: EventDone})
	return nil
}

type streamBridge struct {
	emit func(Event)
}

func (s *streamBridge) OnToken(token string) {
	s.emit(Event{Kind: EventToken, Text: token})
}

func (s *streamBridge) OnToolCallDelta(index int, tc llm.ToolCall) {
	// optional: show streaming tool assembly
	_ = index
	_ = tc
}

func (s *streamBridge) OnStatus(text string) {
	if text != "" {
		s.emit(Event{Kind: EventStatus, Text: text})
	}
}

// afterToolsAnswerHint is injected only into the next model request (not history).
func afterToolsAnswerHint(userQuestion string, toolsUsed int) string {
	if isActionRequest(userQuestion) {
		return fmt.Sprintf(`You have used %d tool call(s). Continue the user's request using ONLY real tool results.
- If file/git changes are still incomplete, call more tools (str_replace/write_file/git).
- If work is done, give a short confirmation of what changed on disk (paths + git result). Do not invent success.
- Do not paste full file bodies or long plans.`, toolsUsed)
	}
	base := `You now have tool results above. Answer the user's latest question using ONLY those results.
Rules:
- Do not invent files, folders, or paths that did not appear in tool output.
- Do not paste large listings or full file bodies unless the user asked to show them.
- Prefer a short direct answer (a few sentences max).`
	uq := strings.TrimSpace(userQuestion)
	low := strings.ToLower(uq)
	// "can you do it" is action, not yes/no — handled above.
	if !isActionRequest(userQuestion) &&
		((strings.Contains(low, "yes") && strings.Contains(low, "no")) ||
			strings.Contains(low, "yes or no") || strings.Contains(low, "y/n") ||
			(strings.HasPrefix(low, "can you") && strings.Contains(low, "read")) ||
			strings.Contains(low, "able to read")) {
		base += "\n- This is a yes/no style question: start with Yes or No, then one short line of detail."
	}
	return base
}

// isLinkCheckRequest detects "check links" style tasks that models mishandle with shell.
func isLinkCheckRequest(user string) bool {
	s := strings.ToLower(strings.TrimSpace(user))
	if s == "" {
		return false
	}
	if strings.Contains(s, "link") && (strings.Contains(s, "check") || strings.Contains(s, "working") ||
		strings.Contains(s, "broken") || strings.Contains(s, "valid") || strings.Contains(s, "verify") ||
		strings.Contains(s, "test") || strings.Contains(s, "all")) {
		return true
	}
	if strings.Contains(s, "urls") && (strings.Contains(s, "check") || strings.Contains(s, "broken")) {
		return true
	}
	return false
}

// isShellOnlyAssistantText is true when the model dumped a shell recipe as its whole reply.
func isShellOnlyAssistantText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	// strip fences
	if strings.HasPrefix(s, "```") {
		body := strings.TrimSpace(strings.TrimPrefix(s, "```"))
		for _, lang := range []string{"bash", "sh", "shell", "zsh"} {
			if strings.HasPrefix(strings.ToLower(body), lang) {
				body = strings.TrimSpace(body[len(lang):])
				break
			}
		}
		if i := strings.Index(body, "```"); i >= 0 {
			body = strings.TrimSpace(body[:i])
		}
		s, low = body, strings.ToLower(body)
	}
	if strings.Contains(low, "xargs") {
		return true
	}
	if strings.Contains(low, "|") && (strings.Contains(low, "grep") || strings.Contains(low, "curl") || strings.Contains(low, "wget")) {
		return true
	}
	for _, p := range []string{"grep ", "rg ", "find ", "xargs ", "curl ", "wget ", "run_shell"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// recoverShellishContent turns bare shell dumps into safe tool calls when possible.
// If the shell would be blocked, returns a note and no calls.
func recoverShellishContent(content string, known map[string]struct{}) ([]llm.ToolCall, string, string) {
	s := strings.TrimSpace(content)
	if s == "" || !isShellOnlyAssistantText(s) {
		return nil, content, ""
	}
	// unwrap fence
	cmd := s
	if strings.HasPrefix(s, "```") {
		body := strings.TrimSpace(strings.TrimPrefix(s, "```"))
		for _, lang := range []string{"bash", "sh", "shell", "zsh"} {
			if strings.HasPrefix(strings.ToLower(body), lang) {
				body = strings.TrimSpace(body[len(lang):])
				break
			}
		}
		if i := strings.Index(body, "```"); i >= 0 {
			body = strings.TrimSpace(body[:i])
		}
		cmd = body
	}
	// Prefer mapping URL-harvest shells to grep tool (safe).
	low := strings.ToLower(cmd)
	if _, ok := known["grep"]; ok && (strings.Contains(low, "http") || strings.Contains(low, "https")) {
		if strings.Contains(low, "grep") || strings.Contains(low, "rg ") || strings.Contains(low, "xargs") {
			args := `{"pattern":"https?://","path":".","max_results":40}`
			tc := llm.ToolCall{
				ID:   fmt.Sprintf("recover_grep_%d", len(cmd)),
				Type: "function",
				Function: llm.FunctionCall{
					Name:      "grep",
					Arguments: args,
				},
			}
			return []llm.ToolCall{tc}, "", "recovered shell dump → grep tool (https?://)"
		}
	}
	if reason := tools.ShellCommandBlocked(cmd); reason != "" {
		return nil, "", "refused shell dump: " + reason
	}
	if _, ok := known["run_shell"]; ok {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		tc := llm.ToolCall{
			ID:       "recover_shell",
			Type:     "function",
			Function: llm.FunctionCall{Name: "run_shell", Arguments: string(b)},
		}
		return []llm.ToolCall{tc}, "", "recovered shell dump → run_shell"
	}
	return nil, content, ""
}

// isActionRequest is true when the user wants real on-disk / git changes, not advice only.
func isActionRequest(user string) bool {
	s := strings.ToLower(strings.TrimSpace(user))
	if s == "" {
		return false
	}
	// Short "do it" / "apply" follow-ups
	switch strings.Join(strings.Fields(strings.Trim(s, "!.?")), " ") {
	case "do it", "do this", "please do it", "go ahead", "apply it", "apply",
		"make the change", "make the changes", "implement it", "just do it",
		"can you do it", "could you do it", "yes do it", "ok do it", "yes apply":
		return true
	}
	needles := []string{
		"do it", "apply the", "apply these", "apply this", "make the change",
		"implement ", "implement it", "write the", "update the readme", "update readme",
		"edit the", "fix the", "create a branch", "create branch", "commit ",
		"git commit", "git push", "push the", "refactor ", "add a section",
		"improve the readme", "improve readme", "please apply", "go ahead and",
		"make these changes", "make those changes",
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func capToolResult(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n…[truncated for model; do not invent the rest]…"
}

// sanitizeToolArgsJSON fixes common model argument shapes so tools run and
// subsequent API rounds don't fail with "invalid tool call arguments".
func sanitizeToolArgsJSON(name, args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "null" {
		return "{}"
	}
	// Already an object
	if strings.HasPrefix(args, "{") && json.Valid([]byte(args)) {
		return args
	}
	// Bare JSON array (git models love this) → wrap
	if strings.HasPrefix(args, "[") {
		if name == "git" {
			// try as args array
			b, err := json.Marshal(map[string]any{"args": json.RawMessage(args)})
			if err == nil {
				return string(b)
			}
		}
		return fmt.Sprintf(`{"args":%s}`, args)
	}
	// Double-encoded object string
	if strings.HasPrefix(args, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(args), &inner); err == nil && strings.TrimSpace(inner) != "" {
			return sanitizeToolArgsJSON(name, inner)
		}
	}
	// Plain text command for git
	if name == "git" {
		b, _ := json.Marshal(map[string]string{"command": args})
		return string(b)
	}
	// Fallback: wrap as content/path-ish
	if !json.Valid([]byte(args)) {
		b, _ := json.Marshal(map[string]string{"input": args})
		return string(b)
	}
	return args
}

// isTrivialChat is true for short greetings / small-talk that should not
// trigger tool schemas (faster first reply over Ollama, local or tunneled).
func isTrivialChat(user string) bool {
	s := strings.TrimSpace(strings.ToLower(user))
	if s == "" {
		return false
	}
	// Strip common punctuation for matching.
	s = strings.Map(func(r rune) rune {
		switch r {
		case '!', '?', '.', ',', ';', ':', '"', '\'':
			return -1
		default:
			return r
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 48 {
		return false
	}
	// Paths / shell-ish → not trivial.
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, needle := range []string{
		"list ", "read ", "write ", "create ", "delete ", "open ", "show ",
		"file", "dir", "folder", "code", "repo", "path", "run ", "exec",
		"cd ", "ls ", "cat ", "grep", "find ", "fix", "function", "bug",
		"error", "fix", "implement", "refactor", "debug",
	} {
		if strings.Contains(s, needle) {
			return false
		}
	}
	switch s {
	case "hi", "hello", "hey", "yo", "sup", "howdy", "hola",
		"hi there", "hello there", "hey there",
		"good morning", "good afternoon", "good evening", "good night",
		"thanks", "thank you", "thx", "ty",
		"ok", "okay", "k", "cool", "nice", "great",
		"bye", "goodbye", "see you", "cya",
		"how are you", "how r you", "whats up", "what's up", "what up",
		"who are you", "what are you", "help":
		return true
	}
	// Very short 1–2 word greetings with common openers.
	words := strings.Fields(s)
	if len(words) <= 2 {
		switch words[0] {
		case "hi", "hello", "hey", "yo", "sup", "howdy", "hola", "thanks", "bye":
			return true
		}
	}
	return false
}
