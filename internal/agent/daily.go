package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// DailyStage is intentionally a human-facing stage label, not a scheduler.
type DailyStage string

const (
	DailyInspect  DailyStage = "inspect"
	DailyPlan     DailyStage = "plan"
	DailyChange   DailyStage = "change"
	DailyVerify   DailyStage = "verify"
	DailyComplete DailyStage = "complete"
	DailyBlocked  DailyStage = "blocked"
)

// EnableDailyMode opts into the supervised local-workbench defaults. It uses
// compatibility execution and never enables the model-facing Goal Graph.
func (a *Agent) EnableDailyMode() {
	a.DailyMode = true
	a.CompatibilityMode = true
	a.DailyStage = DailyInspect
	a.ProviderState = ""
}

func (a *Agent) DisableDailyMode() { a.DailyMode = false }

func (a *Agent) ModeName() string {
	if a.DailyMode {
		return "Daily"
	}
	if a.Scheduler != nil {
		return "Goal Graph"
	}
	if a.CompatibilityMode {
		return "Compatibility"
	}
	return "Legacy"
}

func (a *Agent) dailyStage() DailyStage {
	if !a.DailyMode {
		return ""
	}
	if a.RunState.Phase == PhaseComplete {
		return DailyComplete
	}
	if a.RunState.Phase == PhaseBlocked {
		return DailyBlocked
	}
	if a.RunState.Verification == VerificationPending || a.RunState.Verification == VerificationBlocked || a.RunState.hasMutation() {
		if a.RunState.hasMutation() && !a.RunState.hasSuccessfulVerificationEvidence() {
			return DailyVerify
		}
	}
	if a.RunState.hasMutation() {
		return DailyChange
	}
	if len(a.RunState.Observations) > 0 {
		return DailyPlan
	}
	return DailyInspect
}

func (a *Agent) DailyStageForDisplay() DailyStage { return a.dailyStage() }

// DailyCheckpoint is derived only from the authoritative run state and tool
// ledger. It deliberately excludes model prose and private reasoning.
func (a *Agent) DailyCheckpoint() string {
	if !a.DailyMode {
		return ""
	}
	return "daily checkpoint:\n" + a.FactualSummary()
}

// DailyHandoff is the concise, resumable failure handoff shown after a safe
// stop. It never claims that the task completed.
func (a *Agent) DailyHandoff(reason string) string {
	if !a.DailyMode {
		return ""
	}
	return "daily handoff:\nreason: " + compactStateText(reason, 180) + "\n" + a.FactualSummary()
}

func (a *Agent) FactualSummary() string {
	files := a.ChangedFiles()
	commands := a.CommandsRun()
	remaining := []string{}
	for _, criterion := range a.RunState.AcceptanceCriteriaState {
		if !criterion.Satisfied {
			remaining = append(remaining, criterion.Description)
		}
	}
	for _, criterion := range a.RunState.VerificationCriteria {
		if criterion.Status != VerificationPassed {
			remaining = append(remaining, criterion.Description+"="+string(criterion.Status))
		}
	}
	blocked := []string{}
	for _, failure := range a.RunState.Failures {
		blocked = append(blocked, string(failure.Category)+": "+failure.Summary)
	}
	next := "inspect the listed remaining requirements"
	if len(remaining) == 0 && a.RunState.Verification == VerificationPassed {
		next = "review the final diff and stop"
	} else if a.RunState.hasMutation() {
		next = "run /verify or inspect the changed files"
	} else if len(blocked) > 0 {
		next = "inspect the failure and choose continue, retry verification, revise the plan, or stop"
	}
	return strings.Join([]string{
		"stage: " + string(a.dailyStage()),
		"mode: " + a.ModeName(),
		"model: " + a.Cfg.Model,
		"profile: " + profileName(a.InferenceProfile),
		"workspace: " + a.Cfg.Workspace,
		"provider: " + dailyProviderState(a.ProviderState),
		"provider turn: " + dailyTurnSummary(a.RunState),
		"pending approvals: " + dailyJoinOrNone(a.PendingApprovals),
		"queued requests: " + dailyJoinOrNone(a.PendingRequests),
		"changed files: " + dailyJoinOrNone(files),
		"commands/tests: " + dailyJoinOrNone(commands),
		"verification: " + verificationSummary(a.RunState),
		"remaining: " + dailyJoinOrNone(remaining),
		"blocked: " + dailyJoinOrNone(blocked),
		"next: " + next,
	}, "\n")
}

func dailyTurnSummary(s AgentRunState) string {
	if s.UnknownToolOutcome {
		return "interrupted; unknown action outcome for " + s.ToolInProgress
	}
	if s.Interrupted {
		return "interrupted; context reconstructed before new action"
	}
	if s.ProviderTurnInProgress {
		return "in progress"
	}
	return "idle"
}

// DailyReconnect performs only readiness checks. It never sends a model turn
// and therefore cannot replay a tool action.
func (a *Agent) DailyReconnect(ctx context.Context, emit func(Event)) error {
	if !a.DailyMode {
		return fmt.Errorf("reconnect is available in daily mode only")
	}
	a.ProviderState = llm.ProviderReconnecting
	a.RunState.ProviderState = string(llm.ProviderReconnecting)
	emit(Event{Kind: EventStatus, Text: "provider: reconnecting"})
	err := a.dailyPreflight(ctx, emit)
	if err != nil {
		emit(Event{Kind: EventStatus, Text: "provider: unavailable (" + string(llm.ProviderErrorClassOf(err)) + ")"})
		return err
	}
	a.ProviderState = llm.ProviderResumed
	a.RunState.ProviderState = string(llm.ProviderResumed)
	a.persistDaily()
	emit(Event{Kind: EventStatus, Text: "provider: connected; safe to start a fresh turn"})
	return nil
}

func dailyProviderState(state llm.ProviderState) string {
	if state == "" {
		return string(llm.ProviderUnavailable)
	}
	return string(state)
}

func (a *Agent) dailyPreflight(ctx context.Context, emit func(Event)) error {
	if !a.DailyMode {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		readiness, err := a.Client.DailyReadiness(ctx, a.Cfg.Model)
		if err == nil {
			a.ProviderState = readiness.State
			a.RunState.ProviderState = string(readiness.State)
			emit(Event{Kind: EventStatus, Text: "provider: connected"})
			return nil
		}
		lastErr = err
		class := llm.ProviderErrorClassOf(err)
		if attempt == 0 && dailyTransientProviderError(class) {
			a.ProviderState = llm.ProviderReconnecting
			a.RunState.ProviderState = string(llm.ProviderReconnecting)
			emit(Event{Kind: EventStatus, Text: "provider: reconnecting (" + string(class) + ")"})
			if !dailyBackoff(ctx, attempt) {
				break
			}
			continue
		}
		a.ProviderState = llm.ProviderUnavailable
		a.RunState.ProviderState = string(llm.ProviderUnavailable)
		a.persistDaily()
		return err
	}
	a.ProviderState = llm.ProviderUnavailable
	a.RunState.ProviderState = string(llm.ProviderUnavailable)
	a.persistDaily()
	if lastErr != nil {
		return fmt.Errorf("provider unavailable after bounded readiness retries: %w", lastErr)
	}
	return fmt.Errorf("provider unavailable after bounded readiness retries")
}

func dailyBackoff(ctx context.Context, attempt int) bool {
	d := 100 * time.Millisecond
	if attempt > 0 {
		d = 250 * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dailyChatStream buffers tokens until a request succeeds. A reset during a
// stream therefore cannot duplicate a partial answer when the safe request is
// retried before any tool dispatch.
type dailyChatStream struct {
	emit   func(Event)
	quiet  bool
	tokens strings.Builder
}

func (s *dailyChatStream) OnToken(token string)              { s.tokens.WriteString(token) }
func (s *dailyChatStream) OnToolCallDelta(int, llm.ToolCall) {}
func (s *dailyChatStream) OnStatus(text string) {
	if text != "" {
		s.emit(Event{Kind: EventStatus, Text: text})
	}
}
func (s *dailyChatStream) commit() {
	if !s.quiet && s.tokens.Len() > 0 {
		s.emit(Event{Kind: EventToken, Text: s.tokens.String()})
	}
}

func (a *Agent) dailyChatStream(ctx context.Context, req llm.ChatRequest, emit func(Event), quiet bool) (llm.Message, error) {
	if err := a.dailyPreflight(ctx, emit); err != nil {
		return llm.Message{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		a.RunState.ProviderTurnInProgress = true
		a.RunState.Interrupted = false
		a.persistDaily()
		h := &dailyChatStream{emit: emit, quiet: quiet}
		msg, err := a.Client.ChatStream(ctx, req, h)
		if err == nil {
			h.commit()
			a.RunState.ProviderTurnInProgress = false
			a.RunState.Interrupted = false
			a.ProviderState = llm.ProviderConnected
			a.RunState.ProviderState = string(llm.ProviderConnected)
			a.persistDaily()
			return msg, nil
		}
		class := llm.ProviderErrorClassOf(err)
		lastErr = err
		if attempt == 0 && dailyTransientProviderError(class) {
			a.ProviderState = llm.ProviderReconnecting
			a.RunState.ProviderState = string(llm.ProviderReconnecting)
			emit(Event{Kind: EventStatus, Text: "provider: reconnecting after " + string(class)})
			if !dailyBackoff(ctx, attempt) {
				break
			}
			if err := a.dailyPreflight(ctx, emit); err != nil {
				break
			}
			continue
		}
		a.RunState.ProviderTurnInProgress = false
		a.RunState.Interrupted = true
		a.RunState.Phase = PhaseBlocked
		a.ProviderState = llm.ProviderUnavailable
		a.RunState.ProviderState = string(llm.ProviderUnavailable)
		a.persistDaily()
		return llm.Message{}, err
	}
	a.RunState.ProviderTurnInProgress = false
	a.RunState.Interrupted = true
	a.RunState.Phase = PhaseBlocked
	a.ProviderState = llm.ProviderUnavailable
	a.RunState.ProviderState = string(llm.ProviderUnavailable)
	a.persistDaily()
	if lastErr != nil {
		return llm.Message{}, fmt.Errorf("provider unavailable after bounded reconnect retries: %w", lastErr)
	}
	return llm.Message{}, fmt.Errorf("provider unavailable after bounded reconnect retries")
}

func dailyTransientProviderError(class llm.ProviderErrorClass) bool {
	return class == llm.ProviderConnectionRefused || class == llm.ProviderTimeout || class == llm.ProviderReset || class == llm.ProviderUnavailableClass
}

// CommandsRun returns the authoritative command observations, including
// failed commands, so a handoff never loses the last attempted verification.
func (a *Agent) CommandsRun() []string {
	commands := []string{}
	for _, call := range a.RunState.ToolCalls {
		if call.Name == "run_tests" || call.Name == "run_shell" {
			commands = append(commands, commandFromArgs(call.Arguments))
		}
	}
	return commands
}

func (a *Agent) OpenFailures() []string {
	items := make([]string, 0, len(a.RunState.Failures))
	for _, failure := range a.RunState.Failures {
		items = append(items, string(failure.Category)+": "+failure.Summary)
	}
	return items
}

func (a *Agent) NextAction() string {
	if a.RunState.Phase == PhaseComplete {
		return "review the final diff and stop"
	}
	if len(a.RunState.Failures) > 0 && !a.RunState.lastObservationSuccess() {
		return "inspect the failure and choose continue, retry verification, revise the plan, or stop"
	}
	if a.RunState.hasMutation() {
		return "run /verify or inspect the changed files"
	}
	return "inspect the listed remaining requirements"
}

func (a *Agent) ChangedFiles() []string {
	seen := map[string]bool{}
	files := []string{}
	for _, call := range a.RunState.ToolCalls {
		if call.Outcome != tools.FailureSuccess || call.TargetPath == "" || seen[call.TargetPath] {
			continue
		}
		seen[call.TargetPath] = true
		files = append(files, call.TargetPath)
	}
	sort.Strings(files)
	return files
}

// RunDailyVerification is deterministic and read-only: it can execute only
// the existing verification actions derived from authoritative requirements.
func (a *Agent) RunDailyVerification(ctx context.Context, emit func(Event)) error {
	if !a.DailyMode {
		return fmt.Errorf("daily verification requires daily mode")
	}
	if !a.compatibilityPostflightEligible() {
		emit(Event{Kind: EventStatus, Text: a.DailyHandoff("verification is not currently eligible; implementation or failure recovery remains outstanding")})
		return nil
	}
	a.runCompatibilityPostflight(ctx, emit)
	return nil
}

func commandFromArgs(args string) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(args), &in) == nil && strings.TrimSpace(in.Command) != "" {
		return compactStateText(in.Command, 180)
	}
	return compactStateText(args, 180)
}

func verificationSummary(s AgentRunState) string {
	if len(s.VerificationCriteria) == 0 {
		return string(s.Verification)
	}
	parts := make([]string, 0, len(s.VerificationCriteria))
	for _, criterion := range s.VerificationCriteria {
		parts = append(parts, criterion.Description+"="+string(criterion.Status))
	}
	return strings.Join(parts, "; ")
}

func profileName(profile string) string {
	if strings.TrimSpace(profile) == "" {
		return "generic"
	}
	return profile
}

func dailyJoinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
