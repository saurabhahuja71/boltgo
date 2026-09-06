// Package permissions contains provider-independent Bolt tool approval policy.
package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Mode string

const (
	ModeAsk   Mode = "ask"
	ModeAllow Mode = "allow"
	ModePlan  Mode = "plan"
)

type Level string

const (
	LevelSafe      Level = "safe"
	LevelConfirm   Level = "confirm"
	LevelDangerous Level = "dangerous"
)

type Decision string

const (
	AllowOnce      Decision = "allow_once"
	AllowSession   Decision = "allow_session"
	AllowPermanent Decision = "allow_permanent"
	Deny           Decision = "deny"
)

type Request struct {
	Tool       string
	Capability string
	Level      Level
	Arguments  string
}

// Manager evaluates policy and stores only explicit permanent grants.
type Manager struct {
	Mode      Mode
	StorePath string
	mu        sync.Mutex
	session   map[string]bool
	permanent map[string]bool
}

func New(mode Mode, storePath string) *Manager {
	if mode == "" {
		mode = ModeAllow // Agenterm compatibility when no Bolt config is loaded.
	}
	m := &Manager{Mode: mode, StorePath: storePath, session: map[string]bool{}, permanent: map[string]bool{}}
	m.load()
	return m
}

func (m *Manager) Check(r Request) (Decision, bool) {
	if r.Capability == "" || r.Level == LevelSafe {
		return AllowOnce, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mode := m.Mode
	if mode == ModePlan {
		return Deny, false
	}
	if mode == ModeAllow && r.Level != LevelDangerous {
		return AllowOnce, false
	}
	key := scope(r)
	if m.session[key] || m.permanent[key] {
		return AllowSession, false
	}
	return Deny, true
}

func (m *Manager) Commit(r Request, d Decision) Decision {
	if d == Deny || r.Level == LevelDangerous && d != AllowOnce {
		if d != AllowOnce {
			return Deny
		}
		return d
	}
	key := scope(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch d {
	case AllowSession:
		m.session[key] = true
	case AllowPermanent:
		m.permanent[key] = true
		m.saveLocked()
	}
	return d
}

func (m *Manager) SetMode(mode Mode) {
	if mode != "" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.Mode = mode
	}
}

func (m *Manager) Entries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.permanent))
	for k := range m.permanent {
		out = append(out, k)
	}
	return out
}

func scope(r Request) string {
	return string(r.Level) + "|" + r.Tool + "|" + r.Capability + "|" + r.Arguments
}

func (m *Manager) load() {
	if m.StorePath == "" {
		return
	}
	b, err := os.ReadFile(m.StorePath)
	if err != nil {
		return
	}
	var data struct {
		Permanent map[string]bool `json:"permanent"`
	}
	if json.Unmarshal(b, &data) == nil && data.Permanent != nil {
		m.permanent = data.Permanent
	}
}

func (m *Manager) saveLocked() {
	if m.StorePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.StorePath), 0o755); err != nil {
		return
	}
	b, _ := json.MarshalIndent(struct {
		Permanent map[string]bool `json:"permanent"`
	}{m.permanent}, "", "  ")
	_ = os.WriteFile(m.StorePath, b, 0o600)
}

func DefaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "bolt", "permissions.json")
}

func IsDangerous(tool, args string) bool {
	if tool != "run_shell" && tool != "run_tests" && tool != "git" && tool != "ssh_execute" {
		return false
	}
	s := strings.ToLower(args)
	for _, needle := range []string{"rm -rf /", "mkfs", "fdisk", "shutdown", "reboot", "poweroff", "dd if=", "--force"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func LevelForTool(tool, args string) (Level, string) {
	safe := map[string]bool{"list_dir": true, "read_file": true, "find_files": true, "grep": true, "fetch": true, "repo_map": true, "add_todo": true, "complete_todo": true, "update_todo": true, "list_todos": true}
	if safe[tool] {
		return LevelSafe, ""
	}
	if IsDangerous(tool, args) {
		return LevelDangerous, "tool execution"
	}
	return LevelConfirm, "tool execution"
}
