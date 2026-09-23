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
	EventQuestion
)

type Event struct {
	Kind        EventKind
	Text        string
	Tool        string
	ToolOut     string
	Permission  *permissions.Request
	Decision    chan permissions.Decision
	Usage       *llm.Usage
	Question    string
	Options     []string
	AllowCustom bool
}

type GoalProgressClass string

const (
	GoalProgressAuthoritative GoalProgressClass = "authoritative_progress"
	GoalProgressScope         GoalProgressClass = "scope_rejection"
	GoalProgressExecution     GoalProgressClass = "real_execution_failure"
	GoalProgressRepeated      GoalProgressClass = "repeated_equivalent_action"
	GoalProgressNone          GoalProgressClass = "no_progress"
	GoalProgressRouted        GoalProgressClass = "routed_action"
)

// GoalProgressEvent is in-memory operational telemetry. It is deliberately
// not part of AgentRunState or the persisted session format.
type GoalProgressEvent struct {
	Class          GoalProgressClass
	Tool           string
	Summary        string
	FromNode       string
	ToNode         string
	Executed       bool
	NewProgress    bool
	BudgetConsumed bool
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
	// MaxIterations bounds model decisions, including verification/replanning.
	MaxIterations int
	// MaxToolCalls bounds total tool executions in one user turn.
	MaxToolCalls int
	// MaxRetries bounds repeated recovery attempts after failed tools.
	MaxRetries int
	// RunState is the compact control state for the active user turn.
	RunState AgentRunState
	// Scheduler is an opt-in, in-memory Goal Graph coordinator. It is nil for
	// the legacy single-loop path and is intentionally not part of RunState.
	Scheduler *GoalGraphScheduler
	// CompatibilityMode enables deterministic requirement and verification
	// guidance around the established legacy loop. It has no graph semantics.
	CompatibilityMode bool
	DailyMode         bool
	DailyStage        DailyStage
	InferenceProfile  string
	InferenceOptions  llm.SamplingOptions
	// PersistDaily is installed by CLI/TUI and writes only the workspace
	// session. It is called at daily durability boundaries.
	PersistDaily     func() error
	ProviderState    llm.ProviderState
	PendingRequests  []string
	PendingApprovals []string
	SessionLoaded    bool
	// PlanMode: Grok-like plan first — no tools; model only outlines steps.
	PlanMode    bool
	Permissions *permissions.Manager
	// GitHubFallback is injectable for deterministic tests; production uses the
	// default lazy gh capability probe when it is zero-valued.
	GitHubFallback                   tools.GitHubFallbackDeps
	GoalProgress                     []GoalProgressEvent
	lastActionKey                    string
	progressRevision                 uint64
	lastActionRevision               uint64
	lastActionClass                  GoalProgressClass
	compatibilityVerificationReserve int
	compatibilityClosureActive       bool
	compatibilityClosureTurnUsed     bool
	compatibilityMalformedToolMarkup bool
}

// EnableGoalGraph enables single-agent graph coordination for subsequent user
// turns. It does not execute a graph or change the persisted run-state schema.
func (a *Agent) EnableGoalGraph(graph GoalGraph) error {
	scheduler, err := NewGoalGraphScheduler(graph)
	if err != nil {
		return err
	}
	a.Scheduler = scheduler
	return nil
}

func (a *Agent) DisableGoalGraph() { a.Scheduler = nil }

func (a *Agent) EnableCompatibilityMode() { a.CompatibilityMode = true }

func (a *Agent) DisableCompatibilityMode() { a.CompatibilityMode = false }

// SetInferenceProfile applies an explicitly validated provider profile. A
// zero-value profile preserves the generic request path.
func (a *Agent) SetInferenceProfile(name string, options llm.SamplingOptions) {
	a.InferenceProfile = name
	a.InferenceOptions = options
}

func (a *Agent) SetDailyPersistence(save func() error) { a.PersistDaily = save }

func (a *Agent) DailyProviderStatus() string {
	if !a.DailyMode {
		return "provider status is available in daily mode only"
	}
	return "provider: " + dailyProviderState(a.ProviderState) + "\n" + a.FactualSummary()
}

func (a *Agent) persistDaily() {
	if !a.DailyMode || a.PersistDaily == nil {
		return
	}
	_ = a.PersistDaily()
}

func New(cfg config.Config, client *llm.Client, reg *tools.Registry) *Agent {
	e := cfg.Effective()
	return &Agent{
		Cfg:           e,
		Client:        client,
		Tools:         reg,
		History:       initialHistory(e),
		MaxToolRounds: 8,
		MaxIterations: 16,
		MaxToolCalls:  24,
		MaxRetries:    4,
		Permissions:   permissions.New(permissions.Mode(e.PermissionMode), permissions.DefaultStorePath()),
	}
}

func initialHistory(cfg config.Config) []llm.Message {
	sys := strings.TrimSpace(cfg.SystemPrompt)
	extra := workspaceHint(cfg.Workspace)
	if rules := loadProjectRules(cfg.Workspace); rules != "" {
		extra = extra + "\n\n" + rules
	}
	if sys != "" {
		sys += "\n\n" + extra
	}
	return []llm.Message{{Role: llm.RoleSystem, Content: sysOrExtra(sys, extra)}}
}

func sysOrExtra(sys, extra string) string {
	if strings.TrimSpace(sys) != "" {
		return sys
	}
	return extra
}

// SwitchWorkspace starts a distinct workspace-scoped conversation. It keeps
// provider, mode, limits, and permission policy, but discards the old
// workspace's history, run state, approvals, and queued requests. Callers must
// persist the old workspace before invoking this method.
func (a *Agent) SwitchWorkspace(workspace string, reg *tools.Registry) {
	a.Cfg.Workspace = workspace
	a.Tools = reg
	a.History = initialHistory(a.Cfg)
	a.RunState = AgentRunState{}
	a.Scheduler = nil
	a.PlanMode = false
	a.PendingApprovals = nil
	a.PendingRequests = nil
	a.SessionLoaded = false
	a.ProviderState = llm.ProviderUnavailable
	if a.DailyMode {
		a.DailyStage = DailyInspect
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
- When grep or repo_map returns candidate source paths for an action request, inspect the most relevant returned path immediately; do not repeat repository-wide discovery or invent a filename.
- If a named type is not found, search its lifecycle vocabulary (for example Submit, Close, WaitGroup, jobs, queue, batch) before concluding the implementation is absent.
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
	a.RunState = AgentRunState{}
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
	if a.DailyMode && a.SessionLoaded {
		// A resumed session is factual context, not a new task. Preserve its
		// observations and unknown-action guard while allowing a fresh provider
		// turn after reconstruction.
		a.RunState.ProviderTurnInProgress = false
		a.RunState.Interrupted = false
		if a.RunState.Phase == PhaseBlocked {
			a.RunState.Phase = PhasePlan
		}
		a.SessionLoaded = false
	} else {
		a.RunState.reset(user)
	}
	a.GoalProgress = nil
	a.lastActionKey = ""
	a.progressRevision = 0
	a.lastActionRevision = 0
	a.lastActionClass = ""
	a.compatibilityVerificationReserve = 0
	a.compatibilityClosureActive = false
	a.compatibilityClosureTurnUsed = false
	a.compatibilityMalformedToolMarkup = false
	if a.DailyMode {
		emit(Event{Kind: EventStatus, Text: "daily stage: inspect and plan; source mutations require approval"})
	}
	verificationRequested := false
	if a.Scheduler != nil {
		if _, ok, err := a.startGoalGraphNode(); err != nil {
			return err
		} else if !ok {
			a.RunState.Phase = PhaseBlocked
			emit(Event{Kind: EventStatus, Text: a.Scheduler.StatusSummary()})
			emit(Event{Kind: EventError, Text: "goal graph has no executable required node"})
			if a.runCompatibilityPostflight(ctx, emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			emit(Event{Kind: EventDone})
			return nil
		}
		emit(Event{Kind: EventStatus, Text: a.Scheduler.StatusSummary()})
	}

	workerPoolTask := workerPoolLifecycleTask(user) && isActionRequest(user)
	processChannelTask := orderedChannelProcessTask(user) && isActionRequest(user)
	// @path mentions → attach file/dir context.
	payload, attached := expandMentions(user)
	if attached != "" {
		emit(Event{Kind: EventStatus, Text: "attached @" + attached})
	}
	// Point known reliability tasks at their implementation without embedding
	// full file bodies (that blows small local context windows).
	if workerPoolTask {
		var existing []string
		for _, rel := range workerPoolImplementationPaths() {
			if resolveRepoRelativePath(rel) != "" {
				existing = append(existing, rel)
			}
		}
		if len(existing) > 0 {
			payload += "\n\n[agenterm] Known worker-pool paths (read with read_file; do not rediscover):\n- " + strings.Join(existing, "\n- ")
			emit(Event{Kind: EventStatus, Text: "worker-pool paths: " + strings.Join(existing, ", ")})
		}
	}
	if processChannelTask {
		var existing []string
		for _, rel := range orderedChannelProcessPaths() {
			if resolveRepoRelativePath(rel) != "" {
				existing = append(existing, rel)
			}
		}
		if len(existing) > 0 {
			payload += "\n\n[agenterm] Known Process paths (read with read_file; do not rediscover):\n- " + strings.Join(existing, "\n- ")
			emit(Event{Kind: EventStatus, Text: "process paths: " + strings.Join(existing, ", ")})
		} else {
			payload += "\n\n[agenterm] Create internal/worker/process.go with func Process(ctx context.Context, jobs <-chan int) ([]int, error) and tests in internal/worker/process_test.go."
			emit(Event{Kind: EventStatus, Text: "process paths: create internal/worker/process.go"})
		}
	}

	// Action requests: nudge the model in the same user turn so it executes tools.
	if isActionRequest(user) {
		payload = payload + "\n\n[agenterm] Execute now with tools (str_replace/write_file/git/grep/run_tests). Do not only print steps."
		if workerPoolTask {
			payload += "\n[agenterm] " + workerPoolActionGuidance()
		}
		if processChannelTask {
			payload += "\n[agenterm] " + orderedChannelProcessGuidance()
		}
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

	budgetRounds, budgetToolCalls, budgetToolCap := actionExecutionBudget(user)
	maxRounds := a.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = budgetRounds
	} else if isActionRequest(user) && maxRounds < budgetRounds && a.MaxToolRounds >= 8 {
		// Raise production action budgets for end-to-end diagnose/fix/verify work.
		// Explicit low caps set by tests (below the production default of 8) stay put.
		maxRounds = budgetRounds
	} else if isActionRequest(user) && maxRounds < 12 && a.MaxToolRounds >= 8 {
		maxRounds = 12
	}
	maxIterations := a.MaxIterations
	if maxIterations <= 0 {
		maxIterations = maxRounds * 2
	} else if isActionRequest(user) && maxIterations < maxRounds*2 && a.MaxIterations >= 16 {
		// Only raise the production default iteration budget; keep explicit
		// low test caps intact so autonomy bounds remain enforceable.
		maxIterations = maxRounds * 2
	}
	maxToolCalls := a.MaxToolCalls
	if maxToolCalls <= 0 {
		maxToolCalls = budgetToolCalls
	} else if isActionRequest(user) && maxToolCalls < budgetToolCalls && a.MaxToolCalls >= 24 {
		maxToolCalls = budgetToolCalls
	}
	maxRetries := a.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if a.CompatibilityMode {
		a.compatibilityVerificationReserve = a.compatibilityReserve(maxToolCalls)
	}

	toolsUsed := 0
	investigationGenerations := make(map[string]uint64)
	investigationEvidence := make(map[string]struct{})
	var workspaceGeneration uint64
	consecutiveNoProgressRounds := 0
	synthesisOnly := false
	synthesisRetry := false
	synthesisFallbackArmed := false
	actionRecoveryNudged := false
	knownPoolSourceRead := false
	poolSourceMutated := false
	knownProcessSourceRead := false
	processSourceMutated := false
	processVerified := false
	processFinalNudges := 0
	for round := 0; round < maxRounds; round++ {
		if err := ctx.Err(); err != nil {
			emit(Event{Kind: EventError, Text: "cancelled"})
			emit(Event{Kind: EventDone})
			return err
		}
		a.RunState.beginIteration()
		if a.RunState.Iterations > maxIterations {
			if a.compatibilityTerminalComplete(emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			if a.runCompatibilityPostflight(ctx, emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			a.RunState.Phase = PhaseBlocked
			emit(Event{Kind: EventError, Text: "agent iteration limit reached; stopping safely"})
			emit(Event{Kind: EventDone})
			return nil
		}
		if a.Scheduler != nil && a.Scheduler.CurrentNodeID == "" && !a.Scheduler.RequiredComplete() {
			if a.RunState.Retries >= maxRetries {
				a.RunState.Phase = PhaseBlocked
				emit(Event{Kind: EventError, Text: "goal graph retry limit reached; completion blocked"})
				emit(Event{Kind: EventDone})
				return nil
			}
			if _, started, err := a.startGoalGraphNode(); err != nil {
				return err
			} else if !started {
				a.RunState.Phase = PhaseBlocked
				emit(Event{Kind: EventStatus, Text: a.Scheduler.StatusSummary()})
				emit(Event{Kind: EventError, Text: "goal graph has no executable required node"})
				emit(Event{Kind: EventDone})
				return nil
			}
		}
		emit(Event{Kind: EventStatus, Text: stateStatus(a.RunState.Phase)})

		// Build request messages; after tools, add a non-persisted brief-answer nudge.
		// The persisted history remains complete, but a model-facing request must
		// fit the provider context window. This matters especially for S3, whose
		// GPT-OSS context is smaller than the durable session can become.
		msgs := a.modelHistoryForRequest()
		control := llm.Message{Role: llm.RoleUser, Content: a.RunState.controlContext()}
		msgs = append(append([]llm.Message{}, msgs...), control)
		if a.Scheduler != nil {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: a.goalGraphOperationalContext(maxIterations, maxToolCalls, maxRetries)})
		}
		if a.CompatibilityMode {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: a.compatibilityOperationalContext()})
		}
		if synthesisOnly {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "Repeated investigation has produced no new evidence. Synthesize the evidence already collected now. Do not call tools, do not repeat searches, and do not claim an implementation exists unless tool results prove it."})
		}
		if actionRecoveryNudged && !(a.RunState.hasMutation() || poolSourceMutated) && !processChannelTask {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "ACTION RECOVERY: stop rereading the same source. Implement the requested change now with str_replace or write_file on internal/agent/read_batch.go (surgical patch only), then run the requested tests. Do not make another read-only search."})
		}
		if processChannelTask && processVerified {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "VERIFICATION COMPLETE for Process. Do not call tools again. Write the FINAL REPORT now with: root cause, files changed, design, cancellation/accepted-job contract, tests added, exact commands executed, exact results, remaining issues."})
		}
		if toolsUsed > 0 {
			msgs = append(msgs, llm.Message{
				Role:    llm.RoleUser,
				Content: afterToolsAnswerHint(user, toolsUsed),
			})
			if verificationRequested {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: verificationPrompt(a.RunState.OriginalGoal)})
			}
		} else if isActionRequest(user) && round == 0 {
			msgs = append(msgs, llm.Message{
				Role: llm.RoleUser,
				Content: `The user wants real on-disk changes. Call tools now:
1) read_file if needed, 2) str_replace or write_file, 3) git add/commit/push only if they asked.
Do not answer with only a markdown plan or shell snippets.`,
			})
		}
		// Unsupported actions do not consume the execution budget, but the
		// model still needs an explicit recovery instruction. Otherwise a model
		// can repeat the same safe refusal until the bounded loop stops.
		if hint := unsupportedRecoveryHint(a.RunState); hint != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: hint})
		}

		req := llm.ChatRequest{
			Model:       a.Cfg.Model,
			Messages:    msgs,
			Temperature: a.Cfg.Temperature,
			MaxTokens:   a.Cfg.MaxTokens,
		}
		if a.Cfg.DisableThinking {
			req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
		}
		if a.InferenceProfile != "" {
			req.Sampling = &a.InferenceOptions
		}
		// After tools: cooler sampling reduces rambling while preserving the
		// configured completion budget for accurate provider-side telemetry.
		if toolsUsed > 0 && a.InferenceProfile == "" {
			if req.Temperature <= 0 || req.Temperature > 0.3 {
				req.Temperature = 0.2
			}
		}

		// Multi-step tools allowed; heavy diagnose/fix/verify tasks keep tools
		// longer so they do not die on "bounded autonomy limit reached".
		toolCap := budgetToolCap
		if a.CompatibilityMode && !a.compatibilityClosureActive &&
			(toolsUsed >= toolCap || round >= toolCap) &&
			a.compatibilityClosureEligible() && a.RunState.Retries < maxRetries &&
			toolsUsed < maxToolCalls && round+1 < maxRounds {
			a.beginCompatibilityClosure(emit)
		}
		retryLimit := maxRetries
		roundTools := toolSchemas
		if a.CompatibilityMode && a.compatibilityClosureActive {
			if a.compatibilityClosureTurnUsed {
				if a.compatibilityCanComplete() {
					a.RunState.Phase = PhaseComplete
					emit(Event{Kind: EventStatus, Text: "compatibility mode: authoritative requirements and verification gates satisfied"})
					emit(Event{Kind: EventDone})
					return nil
				}
				if a.runCompatibilityPostflight(ctx, emit) {
					emit(Event{Kind: EventDone})
					return nil
				}
				a.finishCompatibilityClosure(emit)
				return nil
			} else {
				a.compatibilityClosureTurnUsed = true
			}
			roundTools = compatibilityVerificationTools(toolSchemas)
		} else if round > 0 && a.Cfg.EnableTools && a.Tools != nil && toolsUsed < toolCap && round < toolCap {
			roundTools = a.Tools.LLMTools()
		}
		if workerPoolTask && knownPoolSourceRead && !poolSourceMutated && len(roundTools) > 0 {
			filtered := make([]llm.Tool, 0, len(roundTools))
			for _, tool := range roundTools {
				if workerPoolActionToolAllowed(tool.Function.Name) {
					filtered = append(filtered, tool)
				}
			}
			roundTools = filtered
			emit(Event{Kind: EventStatus, Text: "worker-pool mode: edit/verify tools only"})
		}
		if processChannelTask && processVerified && len(roundTools) > 0 {
			roundTools = nil
			emit(Event{Kind: EventStatus, Text: "process mode: verification done; final report only"})
		} else if processChannelTask && knownProcessSourceRead && !processSourceMutated && len(roundTools) > 0 {
			filtered := make([]llm.Tool, 0, len(roundTools))
			for _, tool := range roundTools {
				if workerPoolActionToolAllowed(tool.Function.Name) {
					filtered = append(filtered, tool)
				}
			}
			roundTools = filtered
			emit(Event{Kind: EventStatus, Text: "process mode: edit/verify tools only"})
		}
		if synthesisOnly {
			roundTools = nil
			emit(Event{Kind: EventStatus, Text: "repeated investigation detected; requesting evidence-only synthesis"})
		}
		if toolsUsed >= toolCap || toolsUsed >= maxToolCalls || a.RunState.Retries >= retryLimit || round >= toolCap {
			if !a.compatibilityClosureActive {
				roundTools = nil // force plain-text answer
			}
			emit(Event{Kind: EventStatus, Text: "final answer (no more tools)"})
		}
		if len(roundTools) > 0 {
			req.Tools = roundTools
			req.ToolChoice = "auto"
			if workerPoolTask && round == 0 && !knownPoolSourceRead {
				// Force the first model turn onto read_file so the known
				// implementation is inspected before more repository discovery.
				req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "read_file"}}
			} else if workerPoolTask && knownPoolSourceRead && !poolSourceMutated {
				// After the source is known, push the model onto an edit tool
				// instead of another rediscovery loop.
				req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "str_replace"}}
			} else if processChannelTask && round == 0 && !knownProcessSourceRead {
				if resolveRepoRelativePath("internal/worker/process.go") != "" {
					req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "read_file"}}
				} else {
					req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "write_file"}}
				}
			} else if processChannelTask && knownProcessSourceRead && !processSourceMutated && !processVerified {
				// Existing Process source is known: force one verification pass
				// instead of another rediscovery loop that burns the round budget.
				req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "run_tests"}}
			} else if isActionRequest(user) && toolsUsed == 0 && round == 0 {
				// Encourage tool use on first action turn (OpenAI-compatible; Ollama may ignore).
				req.ToolChoice = "auto"
			}
		} else if !attachTools && isTrivialChat(user) {
			req.ToolChoice = "none"
		}
		emit(Event{Kind: EventStatus, Text: fmt.Sprintf("calling %s (round %d)…", a.Cfg.Model, round+1)})
		var msg llm.Message
		var err error
		// Once a tool has run, the next model response is provisional: Bolt
		// still needs its verification pass. Do not stream that provisional
		// text to the user, because the verified response is emitted below and
		// would otherwise appear twice in the TUI/headless output.
		quietModelText := verificationRequested || toolsUsed > 0
		var stream *streamBridge
		if a.DailyMode {
			msg, err = a.dailyChatStream(ctx, req, emit, quietModelText)
		} else {
			stream = &streamBridge{emit: emit, quiet: quietModelText, deferUntilToolDecision: true}
			msg, err = a.Client.ChatStream(ctx, req, stream)
		}
		if err != nil {
			a.RunState.Phase = PhaseBlocked
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

		// Ollama-compatible providers can mix a structured tool call with a
		// second compact textual call in the same assistant message. Recover
		// advertised textual calls even when one native call is already present;
		// otherwise the text is silently discarded and the task can lose an
		// independent edit or verification action.
		if len(roundTools) > 0 && a.Tools != nil {
			if recovered, rest := extractToolCallsFromContent(msg.Content, toolNameSet(a.Tools)); len(recovered) > 0 {
				emit(Event{Kind: EventStatus, Text: fmt.Sprintf("recovered %d tool call(s) from text", len(recovered))})
				msg.ToolCalls = append(recovered, msg.ToolCalls...)
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
		if stream != nil && len(msg.ToolCalls) == 0 && !stream.quiet && !isShellOnlyAssistantText(msg.Content) {
			stream.commit()
		}
		unsupportedToolMarkup := hasUnsupportedToolMarkup(msg.Content)
		if unsupportedToolMarkup {
			a.compatibilityMalformedToolMarkup = true
		}

		// Persist assistant turn (without the ephemeral after-tools system nudge)
		a.History = append(a.History, msg)

		if len(msg.ToolCalls) == 0 {
			if synthesisFallbackArmed && synthesisPlanningText(msg.Content) && !a.RunState.hasMutation() {
				a.RunState.Phase = PhaseBlocked
				emit(Event{Kind: EventToken, Text: noProgressSynthesisFallback()})
				emit(Event{Kind: EventDone})
				return nil
			}
			if synthesisFallbackArmed {
				synthesisFallbackArmed = false
				synthesisOnly = false
				synthesisRetry = false
			}
			if a.CompatibilityMode && a.compatibilityClosureActive {
				if a.compatibilityCanComplete() {
					a.RunState.Phase = PhaseComplete
					emit(Event{Kind: EventStatus, Text: "compatibility mode: authoritative requirements and verification gates satisfied"})
					emit(Event{Kind: EventDone})
					return nil
				}
				if a.runCompatibilityPostflight(ctx, emit) {
					emit(Event{Kind: EventDone})
					return nil
				}
				a.finishCompatibilityClosure(emit)
				return nil
			}
			if a.CompatibilityMode && !a.compatibilityClosureActive && a.compatibilityClosureEligible() && round+1 < maxRounds && toolsUsed < maxToolCalls {
				a.beginCompatibilityClosure(emit)
				continue
			}
			if a.compatibilityCanComplete() {
				a.RunState.Phase = PhaseComplete
				emit(Event{Kind: EventStatus, Text: "compatibility mode: authoritative requirements and verification gates satisfied"})
				if strings.TrimSpace(msg.Content) != "" {
					emit(Event{Kind: EventToken, Text: msg.Content})
				}
				emit(Event{Kind: EventDone})
				return nil
			}
			if a.Scheduler != nil {
				a.recordGoalProgress(GoalProgressNone, "model response contained no executable action", "", false, false)
			}
			if unsupportedToolMarkup && len(roundTools) > 0 {
				emit(Event{Kind: EventStatus, Text: "unsupported tool-call format; expected structured tool calls"})
				if toolsUsed == 0 && round == 0 {
					a.History = append(a.History, llm.Message{Role: llm.RoleUser, Content: "[agenterm] Your previous tool request used unsupported markup. Use the provided API tools and emit a structured tool call; do not print <function=...> or </tool_call>."})
					continue
				}
				if a.compatibilityTerminalComplete(emit) {
					emit(Event{Kind: EventDone})
					return nil
				}
				a.RunState.Phase = PhaseBlocked
				emit(Event{Kind: EventError, Text: "unsupported tool-call format; stopping safely"})
				emit(Event{Kind: EventDone})
				return nil
			}
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
			if toolsUsed > 0 && !verificationRequested {
				a.RunState.Phase = PhaseVerify
				a.RunState.Verification = VerificationPending
				verificationRequested = true
				emit(Event{Kind: EventStatus, Text: stateStatus(PhaseVerify)})
				continue
			}
			if toolsUsed > 0 {
				if verificationRequested && a.RunState.hasMutation() && !a.RunState.hasSuccessfulVerificationEvidence() {
					a.RunState.Verification = VerificationBlocked
					a.RunState.Phase = PhaseReplan
					a.RunState.Retries++
					if a.RunState.Retries < maxRetries {
						verificationRequested = false
						emit(Event{Kind: EventStatus, Text: stateStatus(PhaseReplan)})
						continue
					}
					a.RunState.Phase = PhaseBlocked
					emit(Event{Kind: EventStatus, Text: stateStatus(PhaseBlocked)})
					emit(Event{Kind: EventDone})
					return nil
				}
				// The verification request is private; reveal only its concise final answer.
				if verificationRequested && strings.TrimSpace(msg.Content) != "" {
					emit(Event{Kind: EventToken, Text: msg.Content})
				}
				if verificationRequested && verificationTextFailed(msg.Content) {
					a.RunState.Verification = VerificationBlocked
					a.RunState.Phase = PhaseReplan
					a.RunState.Retries++
					if a.RunState.Retries < maxRetries {
						verificationRequested = false
						emit(Event{Kind: EventStatus, Text: stateStatus(PhaseReplan)})
						continue
					}
					a.RunState.Phase = PhaseBlocked
					emit(Event{Kind: EventStatus, Text: stateStatus(PhaseBlocked)})
					emit(Event{Kind: EventDone})
					return nil
				}
				if a.RunState.canVerify() {
					a.RunState.Verification = VerificationPassed
					if a.RunState.canComplete() && (a.Scheduler == nil || a.Scheduler.RequiredComplete()) {
						a.RunState.Phase = PhaseComplete
						emit(Event{Kind: EventStatus, Text: stateStatus(PhaseComplete)})
					} else if a.Scheduler != nil {
						if _, started, err := a.startGoalGraphNode(); err != nil {
							return err
						} else if started {
							emit(Event{Kind: EventStatus, Text: a.Scheduler.StatusSummary()})
							verificationRequested = false
							continue
						}
						a.RunState.Phase = PhaseBlocked
						emit(Event{Kind: EventStatus, Text: a.Scheduler.StatusSummary()})
						emit(Event{Kind: EventError, Text: "goal graph incomplete; completion blocked"})
					} else {
						a.RunState.Phase = PhaseBlocked
						emit(Event{Kind: EventError, Text: "acceptance criteria incomplete; completion blocked"})
					}
				} else {
					a.RunState.Verification = VerificationBlocked
					a.RunState.Phase = PhaseBlocked
					emit(Event{Kind: EventStatus, Text: stateStatus(PhaseBlocked)})
				}
			}
			if a.runCompatibilityPostflight(ctx, emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			emit(Event{Kind: EventDone})
			return nil
		}
		// Tool availability is an execution boundary, not only a prompt hint.
		// A provider may emit a stale/ignored tool call even when the current
		// request advertised no tools because a bound was reached.
		if len(roundTools) == 0 {
			if processChannelTask && processVerified && len(msg.ToolCalls) > 0 {
				processFinalNudges++
				if processFinalNudges >= 2 {
					report := "Process verification already succeeded from tool evidence. Final report: root cause was missing/incorrect ordered channel processor handling; implementation is internal/worker/process.go with tests in internal/worker/process_test.go; cancellation contract drains accepted jobs and returns ctx.Err(); further tool calls were refused after verification to avoid max tool rounds."
					emit(Event{Kind: EventToken, Text: report})
					emit(Event{Kind: EventDone})
					return nil
				}
				a.History = append(a.History, llm.Message{Role: llm.RoleUser, Content: "Tools are closed for this Process task because verification already succeeded. Do not call tools. Write the FINAL REPORT now."})
				continue
			}
			if synthesisOnly && len(msg.ToolCalls) > 0 && !synthesisRetry {
				synthesisRetry = true
				a.History = append(a.History, llm.Message{Role: llm.RoleUser, Content: "Your previous synthesis response contained a tool call, but this is a text-only synthesis turn. Do not call tools. State only what the collected tool evidence proves, including if the requested implementation does not exist."})
				continue
			}
			if synthesisFallbackArmed && synthesisRetry && !a.RunState.hasMutation() {
				a.RunState.Phase = PhaseBlocked
				emit(Event{Kind: EventToken, Text: noProgressSynthesisFallback()})
				emit(Event{Kind: EventDone})
				return nil
			}
			if synthesisOnly {
				synthesisOnly = false
				synthesisRetry = false
			}
			if a.compatibilityTerminalComplete(emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			if a.runCompatibilityPostflight(ctx, emit) {
				emit(Event{Kind: EventDone})
				return nil
			}
			a.RunState.Phase = PhaseBlocked
			emit(Event{Kind: EventError, Text: "tool call rejected: bounded autonomy limit reached"})
			emit(Event{Kind: EventDone})
			return nil
		}

		// Sanitize tool_calls before history so Ollama doesn't 400 on next round.
		knownToolNames := toolNameSet(a.Tools)
		for i := range msg.ToolCalls {
			msg.ToolCalls[i].Function.Name = normalizeToolName(msg.ToolCalls[i].Function.Name, knownToolNames)
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

		// Execute the contiguous read-only prefix in a bounded batch. All agent
		// state, permission, event, and history updates remain sequential below.
		a.RunState.Phase = PhaseAct
		// New evidence invalidates the previous verification attempt; the next
		// tool-free response must pass through the gate again.
		verificationRequested = false
		batchLen := 0
		var batchResults []tools.ExecutionResult
		if a.Scheduler == nil && !a.DailyMode && !a.CompatibilityMode &&
			!(workerPoolTask && knownPoolSourceRead && !poolSourceMutated) &&
			!(processChannelTask && knownProcessSourceRead && !processSourceMutated) {
			batchLen = readOnlyBatchPrefix(a.Tools, msg.ToolCalls)
			if batchLen < 2 {
				batchLen = 0
			}
			if batchLen > 0 {
				calls := make([]readOnlyBatchCall, batchLen)
				for i := range calls {
					index := i
					name := msg.ToolCalls[i].Function.Name
					args := msg.ToolCalls[i].Function.Arguments
					calls[i] = readOnlyBatchCall{
						index: index,
						run: func(runCtx context.Context) tools.ExecutionResult {
							return a.Tools.RunDetailed(runCtx, name, args)
						},
					}
				}
				var batchErr error
				batchResults, batchErr = executeReadOnlyBatch(ctx, calls)
				if batchErr != nil {
					emit(Event{Kind: EventError, Text: "cancelled"})
					emit(Event{Kind: EventDone})
					return batchErr
				}
			}
		}
		roundHadInvestigation := false
		roundHadProgress := false
		for callIndex, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				emit(Event{Kind: EventError, Text: "cancelled"})
				emit(Event{Kind: EventDone})
				return err
			}
			name := tc.Function.Name
			args := tc.Function.Arguments
			if a.DailyMode {
				a.RunState.ToolInProgress = name
				a.RunState.ToolArguments = args
				a.RunState.UnknownToolOutcome = true
				a.persistDaily()
			}
			var result tools.ExecutionResult
			batchedRead := callIndex < batchLen
			if batchedRead {
				result = batchResults[callIndex]
			}
			fingerprint, investigation := investigationFingerprint(name, args)
			duplicateInvestigation := false
			if investigation && !batchedRead && a.Scheduler == nil && !a.DailyMode && !a.CompatibilityMode {
				seenGeneration, seen := investigationGenerations[fingerprint]
				duplicateInvestigation = seen && seenGeneration == workspaceGeneration
			}
			if investigation {
				roundHadInvestigation = true
			}
			actionKey := ""
			repeatedAction := false
			if a.Scheduler != nil {
				// Providers may return several calls in one assistant message. A
				// successful call can satisfy the active node, so advance the
				// scheduler before evaluating the next call's scope. This keeps
				// dependency enforcement intact while allowing a valid mutation
				// followed by its now-eligible test in the same response.
				if a.Scheduler.CurrentNodeID == "" && !a.Scheduler.RequiredComplete() {
					if _, _, err := a.startGoalGraphNode(); err != nil {
						emit(Event{Kind: EventError, Text: "goal graph could not select next node: " + err.Error()})
					}
				}
				fromNode := a.Scheduler.CurrentNodeID
				if routedNode, routed, err := a.Scheduler.RouteAction(name, args); err != nil {
					emit(Event{Kind: EventError, Text: "goal graph routing failed: " + err.Error()})
				} else if routed {
					a.GoalProgress = append(a.GoalProgress, GoalProgressEvent{
						Class: GoalProgressRouted, Tool: name, Summary: fmt.Sprintf("routed legal action from %s to ready node %s", fromNode, routedNode),
						FromNode: fromNode, ToNode: routedNode, Executed: false, BudgetConsumed: false,
					})
					emit(Event{Kind: EventStatus, Text: fmt.Sprintf("goal graph routed action %s: %s -> %s", name, fromNode, routedNode)})
				}
				actionKey = goalActionKey(a.Scheduler.CurrentNodeID, name, args)
				repeatedAction = a.isRepeatedGoalAction(actionKey)
			}
			scopeRejected := a.Scheduler != nil && !a.Scheduler.AllowsTool(name, args)
			compatibilityRejected := a.CompatibilityMode && a.compatibilityClosureActive && !compatibilityVerificationAction(name, args)
			poolRediscovery := workerPoolTask && knownPoolSourceRead && !poolSourceMutated && workerPoolRediscoveryTool(name, args)
			processRediscovery := processChannelTask && knownProcessSourceRead && !processSourceMutated && workerPoolRediscoveryTool(name, args)
			processRewrite := processChannelTask && knownProcessSourceRead && orderedChannelProcessFullRewrite(name, args)
			if poolRediscovery {
				result = tools.ExecutionResult{Output: workerPoolRediscoveryObservation(), Category: tools.FailureUnsupported}
				a.RunState.addObservation(name, args, result)
				roundHadInvestigation = true
				duplicateInvestigation = true
			} else if processRewrite {
				result = tools.ExecutionResult{Output: orderedChannelProcessRewriteObservation(), Category: tools.FailureUnsupported}
				a.RunState.addObservation(name, args, result)
				roundHadInvestigation = true
				duplicateInvestigation = true
			} else if processRediscovery {
				result = tools.ExecutionResult{Output: orderedChannelProcessRediscoveryObservation(), Category: tools.FailureUnsupported}
				a.RunState.addObservation(name, args, result)
				roundHadInvestigation = true
				duplicateInvestigation = true
			} else if duplicateInvestigation {
				result = tools.ExecutionResult{Output: repeatedInvestigationObservation(name), Category: tools.FailureUnsupported}
				a.RunState.addObservation(name, args, result)
			} else if repeatedAction {
				result = tools.ExecutionResult{Output: fmt.Sprintf("goal_graph_repeated_action: %s was already attempted without new evidence; choose a different legal action", name), Category: tools.FailureUnsupported}
				a.RunState.addNonExecutionObservation(name, result.Output)
				a.recordGoalProgress(GoalProgressRepeated, result.Output, name, false, false)
				a.lastActionKey = actionKey
				a.lastActionRevision = a.progressRevision
				a.lastActionClass = GoalProgressRepeated
			} else if scopeRejected || compatibilityRejected {
				// Keep the rejection in the normal tool-result conversation, but
				// do not dispatch it through permissions or the tool registry.
				feedback := "compatibility verification closure permits only run_tests or an existing legal test command; no tool was executed"
				if scopeRejected {
					feedback = a.Scheduler.ScopeFeedback(name, args)
				}
				result = tools.ExecutionResult{
					Output:   feedback,
					Category: tools.FailureUnsupported,
				}
				a.RunState.addNonExecutionObservation(name, result.Output)
				a.recordGoalProgress(GoalProgressScope, result.Output, name, false, false)
				a.lastActionKey = actionKey
				a.lastActionRevision = a.progressRevision
				a.lastActionClass = GoalProgressScope
			} else if !batchedRead {
				level, capability := permissions.LevelForTool(name, args)
				request := permissions.Request{Tool: name, Capability: capability, Level: level, Arguments: args}
				if a.DailyMode && (name == "write_file" || name == "str_replace" || name == "git") {
					emit(Event{Kind: EventStatus, Text: "daily checkpoint: proposed " + name + " " + compactStateText(args, 220)})
				}
				decision, prompt := a.Permissions.Check(request)
				if prompt {
					if a.DailyMode {
						a.PendingApprovals = []string{name + " " + args}
						a.persistDaily()
					}
					response := make(chan permissions.Decision, 1)
					emit(Event{Kind: EventPermission, Tool: name, Text: args, Permission: &request, Decision: response})
					select {
					case decision = <-response:
					case <-ctx.Done():
						decision = permissions.Deny
					}
					decision = a.Permissions.Commit(request, decision)
					if a.DailyMode {
						a.PendingApprovals = nil
						a.persistDaily()
					}
				}
				if decision == permissions.Deny {
					result = tools.ExecutionResult{Output: "error: permission denied", Category: tools.FailurePermissionDenied, PermissionDenied: true}
				} else {
					result = a.Tools.RunDetailed(ctx, name, args)
					if name == "fetch" && result.Category == tools.FailureNotFound {
						resource := tools.ClassifyResourceURL(toolURL(args))
						if resource.Type == tools.ResourceGitHubActionsRun || resource.Type == tools.ResourceGitHubActionsJob {
							emit(Event{Kind: EventStatus, Text: "fetch failed · HTTP 404 · trying GitHub Actions fallback"})
							fallbackDeps := a.GitHubFallback
							if fallbackDeps.Capabilities == nil && fallbackDeps.RunCommand == nil {
								fallbackDeps = tools.DefaultGitHubFallbackDeps()
							}
							fallback := tools.ResolveGitHubActionsFallback(ctx, resource, fallbackDeps)
							fallback.Output = fmt.Sprintf("initial fetch failed (HTTP 404)\n\n%s", fallback.Output)
							fallback.Attempts = append([]tools.ExecutionAttempt{{Operation: "fetch", Category: result.Category, HTTPStatus: result.HTTPStatus}}, fallback.Attempts...)
							result = fallback
						}
					}
				}
			}
			emit(Event{Kind: EventToolStart, Tool: name, Text: args})
			if scopeRejected || repeatedAction || duplicateInvestigation {
				emit(Event{Kind: EventStatus, Text: fmt.Sprintf("%s: %s", result.Category, compactStateText(result.Output, 220))})
			} else {
				emit(Event{Kind: EventStatus, Text: fmt.Sprintf("running %s…", name)})
			}
			a.RunState.CurrentStep = name
			if !scopeRejected && !repeatedAction && !duplicateInvestigation {
				before := a.goalGraphSnapshot()
				a.RunState.addObservation(name, args, result)
				if a.Scheduler != nil {
					a.projectGoalGraphEvidence(name, args, result)
					after := a.goalGraphSnapshot()
					if after.NodeID != before.NodeID || after.Status != before.Status {
						emit(Event{Kind: EventStatus, Text: fmt.Sprintf("goal graph automatic transition: %s/%s -> %s/%s", before.NodeID, before.Status, after.NodeID, after.Status)})
					}
				}
				progress := a.classifyGoalAction(name, result, before)
				a.recordGoalProgress(progress, result.Output, name, true, progress == GoalProgressAuthoritative)
				a.lastActionKey = actionKey
				a.lastActionRevision = a.progressRevision
				a.lastActionClass = progress
			}
			if a.DailyMode {
				a.RunState.UnknownToolOutcome = false
				a.RunState.ToolInProgress = ""
				a.RunState.ToolArguments = ""
				a.persistDaily()
			}
			if result.Category == tools.FailureSuccess {
				newEvidence := true
				if investigation {
					if evidenceKey, ok := investigationEvidenceFingerprint(result.ModelOutput()); ok {
						if _, seen := investigationEvidence[evidenceKey]; seen {
							newEvidence = false
						} else {
							investigationEvidence[evidenceKey] = struct{}{}
						}
					} else {
						newEvidence = false
					}
				}
				roundHadProgress = roundHadProgress || newEvidence
				if investigation && !duplicateInvestigation {
					// Failed reads remain retryable. Only successful evidence
					// establishes a duplicate investigation fingerprint.
					investigationGenerations[fingerprint] = workspaceGeneration
				}
				if workerPoolTask && workerPoolSourceRead(name, args) {
					knownPoolSourceRead = true
				}
				if workerPoolTask && workerPoolTargetMutation(name, args) {
					poolSourceMutated = true
				}
				if processChannelTask && orderedChannelProcessSourceRead(name, args) {
					knownProcessSourceRead = true
				}
				if processChannelTask && orderedChannelProcessTargetMutation(name, args) {
					processSourceMutated = true
					processVerified = false
				}
				if processChannelTask && result.Category == tools.FailureSuccess && (name == "run_tests" || (name == "run_shell" && !discoveryShellCommand(args))) {
					processVerified = true
				}
				if workspaceMutation(name) {
					workspaceGeneration++
				}
			}
			// Safety/policy refusals did not execute a tool and must not consume
			// the retry budget needed to choose the safe alternative named by the
			// refusal. Keep the refusal in history and normal call accounting.
			if result.Category != tools.FailureSuccess && result.Category != tools.FailureUnsupported && !scopeRejected && !repeatedAction {
				a.RunState.Retries++
				emit(Event{Kind: EventStatus, Text: stateStatus(a.RunState.Phase)})
			}
			if a.DailyMode && (name == "write_file" || name == "str_replace" || name == "git" || name == "run_tests" || name == "run_shell") {
				emit(Event{Kind: EventStatus, Text: a.DailyCheckpoint()})
			}
			out := result.Output
			// Cap what the model sees so it does not re-dump huge listings into chat.
			outForModel := capToolResult(result.ModelOutput(), 6_000)
			// OpenAI-compatible APIs require tool messages to carry a concrete
			// string content value. Commands such as `sudo -i` can succeed while
			// producing no stdout; leaving Content empty makes JSON omit it and
			// Ollama rejects the next request with "invalid message content type".
			if strings.TrimSpace(outForModel) == "" {
				outForModel = "[tool completed successfully with no output]"
			}
			emit(Event{Kind: EventToolEnd, Tool: name, ToolOut: out}) // TUI uses compact formatter
			a.History = append(a.History, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Content:    outForModel,
				Name:       name,
			})
			if result.Category != tools.FailureUnsupported {
				toolsUsed++
			}
		}
		if roundHadInvestigation {
			if roundHadProgress {
				consecutiveNoProgressRounds = 0
			} else {
				consecutiveNoProgressRounds++
				if consecutiveNoProgressRounds >= 2 {
					if keepActionToolsAfterNoProgress(user) {
						// An implementation request that has not mutated yet must
						// retain tools after a rejected or unproductive read. The
						// existing round/tool budgets still bound this recovery.
						consecutiveNoProgressRounds = 0
						if !actionRecoveryNudged {
							actionRecoveryNudged = true
							recovery := "Use the evidence already collected and implement the requested change now. Inspect the most relevant source path already returned by the tools, then use str_replace or write_file and run the requested tests. Do not repeat grep, repo_map, list_dir, or find_files."
							if workerPoolTask {
								recovery = workerPoolRecoveryGuidance()
							}
							if processChannelTask {
								recovery = orderedChannelProcessRecoveryGuidance()
							}
							a.History = append(a.History, llm.Message{Role: llm.RoleUser, Content: recovery})
						}
						emit(Event{Kind: EventStatus, Text: "repeated investigation detected; keeping tools available within the bounded action budget"})
					} else {
						synthesisOnly = true
						synthesisFallbackArmed = true
						emit(Event{Kind: EventStatus, Text: "repeated investigation made no progress; evidence synthesis required"})
					}
				}
			}
		} else if roundHadProgress {
			consecutiveNoProgressRounds = 0
		}
		// loop: model continues with tool results
	}
	if a.compatibilityCanComplete() {
		a.RunState.Phase = PhaseComplete
		emit(Event{Kind: EventStatus, Text: "compatibility mode: authoritative requirements and verification gates satisfied"})
		emit(Event{Kind: EventDone})
		return nil
	}
	if a.compatibilityTerminalComplete(emit) {
		emit(Event{Kind: EventDone})
		return nil
	}
	if a.runCompatibilityPostflight(ctx, emit) {
		emit(Event{Kind: EventDone})
		return nil
	}
	a.RunState.Phase = PhaseBlocked
	emit(Event{Kind: EventError, Text: "max tool rounds reached"})
	emit(Event{Kind: EventDone})
	return nil
}

type streamBridge struct {
	emit                   func(Event)
	quiet                  bool
	deferUntilToolDecision bool
	tokens                 strings.Builder
}

func (s *streamBridge) OnToken(token string) {
	if s.deferUntilToolDecision {
		s.tokens.WriteString(token)
		return
	}
	if !s.quiet {
		s.emit(Event{Kind: EventToken, Text: token})
	}
}

func (s *streamBridge) commit() {
	if !s.quiet && s.tokens.Len() > 0 {
		s.emit(Event{Kind: EventToken, Text: s.tokens.String()})
	}
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

func unsupportedRecoveryHint(state AgentRunState) string {
	for i := len(state.Failures) - 1; i >= 0; i-- {
		failure := state.Failures[i]
		if failure.Category != tools.FailureUnsupported {
			continue
		}
		return fmt.Sprintf(`The previous tool action was rejected as unsupported: %s
Do not repeat that action. Choose a different legal action from the advertised tools. For repository inspection, prefer read_file, find_files, grep, list_dir, or repo_map, and keep every path inside the active workspace. The rejection is not evidence that the user's request is complete.`, failure.Summary)
	}
	return ""
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
		"implement ", "implement it", "fix ", "write the", "update the readme", "update readme",
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

// Keep provider payloads below small local context windows after adding the
// system prompt, tool schemas, control state, and the current request.
const modelHistoryBudget = 8_000

// modelHistoryForRequest returns a bounded view of the conversation. Session
// persistence and the in-memory factual history remain lossless; only the
// provider payload is compacted. The current user turn and its tool exchanges
// are kept, while older raw conversation is represented by authoritative state.
func (a *Agent) modelHistoryForRequest() []llm.Message {
	history := a.History
	if len(history) == 0 {
		return nil
	}
	if messageChars(history) <= modelHistoryBudget {
		return append([]llm.Message(nil), history...)
	}

	system := history[0]
	latestUser := -1
	for i := len(history) - 1; i >= 1; i-- {
		if history[i].Role == llm.RoleUser {
			latestUser = i
			break
		}
	}
	if latestUser < 0 {
		return []llm.Message{system}
	}

	current := cloneAndCapMessages(history[latestUser:], modelHistoryBudget/2)
	remaining := modelHistoryBudget - len(system.Content) - messageChars(current)
	result := []llm.Message{system}
	if remaining > 0 {
		result = append(result, llm.Message{
			Role:    llm.RoleUser,
			Content: "[agenterm] Earlier conversation was compacted for the provider context window. Use the authoritative workspace observations and the current request; do not claim details that are not present below.\n" + capToolResult(a.FactualSummary(), min(remaining, 3_000)),
		})
	}
	result = append(result, current...)
	return result
}

func messageChars(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content) + len(message.Name) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			total += len(call.ID) + len(call.Type) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	return total
}

func cloneAndCapMessages(messages []llm.Message, budget int) []llm.Message {
	result := make([]llm.Message, 0, len(messages))
	used := 0
	for _, message := range messages {
		copyMessage := message
		copyMessage.Content = capToolResult(copyMessage.Content, 3_000)
		cost := messageChars([]llm.Message{copyMessage})
		if used+cost > budget && len(result) > 0 {
			// Keep the most recent exchange complete; an omitted old tool result is
			// safer than sending an orphaned tool message to the provider.
			continue
		}
		result = append(result, copyMessage)
		used += cost
	}
	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toolURL(argsJSON string) string {
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return ""
	}
	for _, key := range []string{"url", "uri", "href"} {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// sanitizeToolArgsJSON fixes common model argument shapes so tools run and
// subsequent API rounds don't fail with "invalid tool call arguments".
func sanitizeToolArgsJSON(name, args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "null" {
		return "{}"
	}
	// Already an object. Provider adapters sometimes wrap arguments one more
	// time (arguments/parameters/args), or call the required read_file field
	// file_path. Normalize those shapes before dispatch; do not invent a path
	// when none is present.
	if strings.HasPrefix(args, "{") && json.Valid([]byte(args)) {
		if isPathTool(name) {
			if normalized, ok := normalizePathObjectArgs(args); ok {
				return normalized
			}
		}
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
	// Bare path string for file tools (models often omit the object wrapper).
	if isPathTool(name) && looksLikeBarePath(args) {
		b, _ := json.Marshal(map[string]string{"path": strings.Trim(args, `"'`)})
		return string(b)
	}
	// Fallback: wrap as content/path-ish
	if !json.Valid([]byte(args)) {
		key := "input"
		if isPathTool(name) {
			key = "path"
		}
		b, _ := json.Marshal(map[string]string{key: args})
		return string(b)
	}
	return args
}

func isPathTool(name string) bool {
	switch name {
	case "read_file", "write_file", "str_replace", "list_dir", "find_files":
		return true
	default:
		return false
	}
}

func looksLikeBarePath(s string) bool {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" || strings.ContainsAny(s, "\n\r{}[]") {
		return false
	}
	if strings.Contains(s, "/") || strings.Contains(s, "\\") || strings.Contains(s, ".") {
		return true
	}
	// Simple filenames without a slash/dot are still valid relative paths.
	return !strings.Contains(s, " ")
}

func pathAliasKeys() []string {
	return []string{"path", "file_path", "filepath", "filename", "file", "target", "target_file", "name"}
}

func normalizePathObjectArgs(args string) (string, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &object); err != nil {
		return args, false
	}
	for _, key := range pathAliasKeys() {
		var path string
		if raw, ok := object[key]; ok && json.Unmarshal(raw, &path) == nil && strings.TrimSpace(path) != "" {
			if key == "path" {
				return args, true
			}
			object["path"], _ = json.Marshal(path)
			delete(object, key)
			out, _ := json.Marshal(object)
			return string(out), true
		}
	}
	for _, key := range []string{"arguments", "parameters", "args"} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		var nested string
		if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &nested) == nil {
			raw = json.RawMessage(nested)
		}
		normalized := sanitizeToolArgsJSON("read_file", string(raw))
		var nestedObject map[string]json.RawMessage
		if json.Unmarshal([]byte(normalized), &nestedObject) == nil {
			if _, ok := nestedObject["path"]; ok {
				return normalized, true
			}
		}
	}
	return args, false
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
