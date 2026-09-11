package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

// SessionMeta is stored next to transcript JSON.
type SessionMeta struct {
	ID               string              `json:"id"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
	Model            string              `json:"model"`
	Summary          string              `json:"summary,omitempty"`
	InferenceProfile string              `json:"inference_profile,omitempty"`
	InferenceOptions llm.SamplingOptions `json:"inference_options,omitempty"`
	Mode             string              `json:"mode,omitempty"`
	Workspace        string              `json:"workspace,omitempty"`
	PermissionMode   string              `json:"permission_mode,omitempty"`
	ChangedFiles     []string            `json:"changed_files,omitempty"`
	Commands         []string            `json:"commands,omitempty"`
	Verification     string              `json:"verification,omitempty"`
	OpenFailures     []string            `json:"open_failures,omitempty"`
	NextAction       string              `json:"next_action,omitempty"`
	ProviderState    string              `json:"provider_state,omitempty"`
	Interrupted      bool                `json:"interrupted,omitempty"`
	UnknownAction    string              `json:"unknown_action,omitempty"`
	PendingApprovals []string            `json:"pending_approvals,omitempty"`
	PendingRequests  []string            `json:"pending_requests,omitempty"`
}

// SessionsDir returns ~/.agenterm/sessions
func SessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agenterm", "sessions"), nil
}

// SaveSession writes conversation history (excluding huge tool blobs truncated).
func (a *Agent) SaveSession(id string) (string, error) {
	dir, err := SessionsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if id == "" {
		id = time.Now().Format("20060102-150405")
	}
	path := filepath.Join(dir, id+".json")
	return a.saveSessionPath(path, id)
}

// SaveSessionPath persists to an explicit workspace-owned path.
func (a *Agent) SaveSessionPath(path string) (string, error) {
	return a.saveSessionPath(path, filepath.Base(path))
}

// WorkspaceSessionPath returns a session path confined to workspace/.bolt/sessions.
// It rejects traversal, absolute IDs, and symlinked session directories that
// resolve outside the selected workspace.
func WorkspaceSessionPath(workspace, id string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("workspace required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = "latest"
	}
	if filepath.Base(id) != id {
		return "", fmt.Errorf("invalid session id")
	}
	id = strings.TrimSuffix(id, ".json")
	if id == "" || id == "." || id == ".." {
		return "", fmt.Errorf("invalid session id")
	}
	path := filepath.Join(workspace, ".bolt", "sessions", id+".json")
	if err := ValidateWorkspaceSessionPath(workspace, path); err != nil {
		return "", err
	}
	return path, nil
}

// ValidateWorkspaceSessionPath detects an existing symlinked ancestor or
// resolved target outside the selected workspace.
func ValidateWorkspaceSessionPath(workspace, path string) error {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(root, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("session path escapes workspace")
	}
	resolvedRoot := root
	if real, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = real
	}
	existing := target
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedExisting)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("session path resolves outside workspace")
	}
	return nil
}

func (a *Agent) saveSessionPath(path, id string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	type wire struct {
		Meta     SessionMeta   `json:"meta"`
		Messages []llm.Message `json:"messages"`
		RunState AgentRunState `json:"run_state,omitempty"`
	}
	msgs := make([]llm.Message, 0, len(a.History))
	for _, m := range a.History {
		cp := m
		if len(cp.Content) > 50_000 {
			cp.Content = cp.Content[:50_000] + "\n…[truncated for session save]…"
		}
		msgs = append(msgs, cp)
	}
	w := wire{
		Meta: SessionMeta{
			ID:               id,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
			Model:            a.Cfg.Model,
			Summary:          a.FactualSummary(),
			InferenceProfile: a.InferenceProfile,
			InferenceOptions: a.InferenceOptions,
			Mode:             a.ModeName(),
			Workspace:        a.Cfg.Workspace,
			PermissionMode:   a.Cfg.PermissionMode,
			ChangedFiles:     a.ChangedFiles(),
			Commands:         a.CommandsRun(),
			Verification:     verificationSummary(a.RunState),
			OpenFailures:     a.OpenFailures(),
			NextAction:       a.NextAction(),
			ProviderState:    dailyProviderState(a.ProviderState),
			Interrupted:      a.RunState.Interrupted,
			UnknownAction:    a.RunState.ToolInProgress,
			PendingApprovals: append([]string(nil), a.PendingApprovals...),
			PendingRequests:  append([]string(nil), a.PendingRequests...),
		},
		Messages: msgs,
		RunState: a.RunState,
	}
	// preserve created_at if file exists
	if data, err := os.ReadFile(path); err == nil {
		var old wire
		if json.Unmarshal(data, &old) == nil && !old.Meta.CreatedAt.IsZero() {
			w.Meta.CreatedAt = old.Meta.CreatedAt
		}
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// LoadSession replaces history with a saved session (keeps tools/client).
func (a *Agent) LoadSession(id string) error {
	dir, err := SessionsDir()
	if err != nil {
		return err
	}
	path := id
	if !filepath.IsAbs(path) && !fileExists(path) {
		path = filepath.Join(dir, id)
		if !fileExists(path) && !fileExists(path+".json") {
			return fmt.Errorf("session not found: %s", id)
		}
		if fileExists(path + ".json") {
			path = path + ".json"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var w struct {
		Meta     SessionMeta   `json:"meta"`
		Messages []llm.Message `json:"messages"`
		RunState AgentRunState `json:"run_state"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if len(w.Messages) == 0 {
		return fmt.Errorf("empty session")
	}
	a.History = w.Messages
	a.RunState = restoredRunState(w.RunState, w.Messages)
	a.restoreSessionFacts(w.Meta)
	return nil
}

// LoadSessionPath loads an explicit session file without consulting ~/.agenterm.
func (a *Agent) LoadSessionPath(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var w struct {
		Meta     SessionMeta   `json:"meta"`
		Messages []llm.Message `json:"messages"`
		RunState AgentRunState `json:"run_state"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if len(w.Messages) == 0 {
		return fmt.Errorf("empty session")
	}
	a.History = w.Messages
	a.RunState = restoredRunState(w.RunState, w.Messages)
	a.restoreSessionFacts(w.Meta)
	return nil
}

func (a *Agent) restoreSessionFacts(meta SessionMeta) {
	a.PendingApprovals = append([]string(nil), meta.PendingApprovals...)
	a.PendingRequests = append([]string(nil), meta.PendingRequests...)
	if !a.DailyMode {
		return
	}
	// A process cannot know whether an in-flight provider request dispatched a
	// tool. Resume must therefore treat it as interrupted and require inspection
	// before any repeat. The persisted tool marker is intentionally retained.
	if a.RunState.ProviderTurnInProgress {
		a.RunState.ProviderTurnInProgress = false
		a.RunState.Interrupted = true
		a.RunState.Phase = PhaseBlocked
	}
	if a.RunState.ToolInProgress != "" {
		a.RunState.UnknownToolOutcome = true
		a.RunState.Interrupted = true
		a.RunState.Phase = PhaseBlocked
	}
	a.ProviderState = llm.ProviderResumed
	a.RunState.ProviderState = string(llm.ProviderResumed)
	a.SessionLoaded = true
}

func restoredRunState(state AgentRunState, messages []llm.Message) AgentRunState {
	if strings.TrimSpace(state.OriginalGoal) != "" {
		return state
	}
	// Older sessions had no control state. Recover the goal from the latest
	// user message without pretending that verification or progress occurred.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleUser {
			state.OriginalGoal = strings.TrimSpace(messages[i].Content)
			state.Phase = PhasePlan
			state.Verification = VerificationNotRun
			return state
		}
	}
	return state
}

// ListSessions returns recent session basenames.
func ListSessions(limit int) ([]string, error) {
	dir, err := SessionsDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	var out []string
	for i := len(ents) - 1; i >= 0 && len(out) < limit; i-- {
		name := ents[i].Name()
		if filepath.Ext(name) == ".json" {
			out = append(out, name)
		}
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func firstUserSnippet(hist []llm.Message) string {
	for _, m := range hist {
		if m.Role == llm.RoleUser {
			s := stringsTrim(m.Content, 80)
			return s
		}
	}
	return ""
}

func stringsTrim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
