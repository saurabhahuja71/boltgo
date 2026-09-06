package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the user-facing agenterm settings file.
type Config struct {
	PermissionMode string `toml:"permission_mode"`
	VisionEnabled  bool   `toml:"vision_enabled"`
	// Workspace is the user project root used by all filesystem and process tools.
	// It is intentionally independent from the directory containing the Bolt binary.
	Workspace string `toml:"workspace"`
	// Provider selects a named block under [providers.*], or "custom".
	Provider string `toml:"provider"`

	// Model is the chat model id (Ollama tag, OpenAI model, xAI model, …).
	Model string `toml:"model"`

	// BaseURL is the OpenAI-compatible API root, e.g.
	//   http://127.0.0.1:11434/v1   (local Ollama)
	//   http://192.168.1.10:11434/v1 (remote Ollama on LAN)
	//   http://127.0.0.1:30000/v1   (SGLang, local or SSH tunnel)
	//   https://api.x.ai/v1
	//   https://api.openai.com/v1
	BaseURL string `toml:"base_url"`

	// APIKey optional for Ollama/SGLang; required for cloud providers.
	// Prefer env AGENTERM_API_KEY / OLLAMA_API_KEY / XAI_API_KEY / OPENAI_API_KEY.
	APIKey string `toml:"api_key"`

	// SystemPrompt prepended as a system message.
	SystemPrompt string `toml:"system_prompt"`

	// Temperature sampling (0–2). Ollama/OpenAI compatible.
	Temperature float64 `toml:"temperature"`

	// MaxTokens completion budget (0 = provider default).
	MaxTokens int `toml:"max_tokens"`

	// EnableTools allows function/tool calling when the model supports it.
	EnableTools bool `toml:"enable_tools"`

	// EnableShell allows run_shell (bash, curl, wget, scripts). Default true.
	EnableShell bool `toml:"enable_shell"`

	// TestCommand default for run_tests tool (empty = auto-detect go test / make test).
	TestCommand string `toml:"test_command"`

	// MCPServers optional external MCP tool servers.
	MCPServers []MCPServer `toml:"mcp_servers"`

	// Providers optional named presets (ollama-local, ollama-remote, sglang, xai, …).
	Providers map[string]Provider `toml:"providers"`
}

// Provider is a reusable endpoint preset.
type Provider struct {
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	Model   string `toml:"model"`
}

// MCPServer describes how to attach an MCP tool server.
type MCPServer struct {
	Name    string `toml:"name"`
	Enabled bool   `toml:"enabled"`
	// URL for streamable HTTP, e.g. http://127.0.0.1:8080/mcp
	URL string `toml:"url"`
	// Command+Args for stdio transport (local process).
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// Default returns sensible local-Ollama defaults.
func Default() Config {
	return Config{
		PermissionMode: "ask",
		VisionEnabled:  false,
		Workspace:      "",
		Provider:       "ollama-local",
		// Best local default for Go + docs + tools (see docs/grok-parity-roadmap.md).
		Model:       "qwen2.5-coder:32b",
		BaseURL:     "http://127.0.0.1:11434/v1",
		APIKey:      "ollama",
		Temperature: 0.7,
		EnableTools: true,
		// Shell on by default so curl/wget/scripts work; set enable_shell=false or --no-shell to disable.
		EnableShell: true,
		SystemPrompt: `You are agenterm, a fast terminal coding assistant that CAN change files on disk.

Style:
- Be concise. Prefer short answers.
- Yes/no questions: answer Yes or No in the first sentence, then at most 1–2 short lines.
- Do NOT invent files, directories, paths, or file listings. If tools were not used or failed, say so.
- Do NOT paste long directory listings or full file contents into the chat unless the user asked to show them.
- After tools return, summarize only what is needed for the user's question.

EXECUTE vs DESCRIBE (critical):
- If the user asks you to do / apply / implement / fix / edit / update / create / write / improve / commit / push changes:
  you MUST use tools (str_replace, write_file, git, run_shell). Printing shell steps alone is NOT enough.
- Prefer str_replace for partial edits; write_file for new files or full rewrites.
- Use git tool for branch/add/commit/push when the user wants git ops.
- After applying, confirm with real tool results (e.g. "wrote N bytes", "updated path").

Tools (list_dir, read_file, write_file, str_replace, find_files, git, fetch, run_shell, …):
- Do NOT call tools for greetings or small talk.
- When the user asks about a repo, README, file, or folder: use tools; never guess contents.
- Paths are relative to the workspace cwd (see workspace hint). Never invent roots like "repo/".
- Prefer run_shell for kubectl, watch, logs, and similar commands so they run in the current shell. Check the current hostname, whoami, and kubectl context before using ssh_execute; use SSH only when the target is clearly another machine.
- If SSH reports "Permission denied", do not retry. Explain that SSH authentication failed, then give the exact command for the user to run locally when the current system appears to be the target.
- Link checks: grep/read_file for http(s) URLs in the repo, then call fetch once per URL (limit ~15). NEVER use xargs+curl/wget or site crawls via run_shell.
- HTTP GET: prefer fetch. Scripts: run_shell with bash script.sh (one short command).
- Prefer the smallest useful tool action.`,
		Providers: map[string]Provider{
			"ollama-local": {
				BaseURL: "http://127.0.0.1:11434/v1",
				APIKey:  "ollama",
				Model:   "qwen2.5-coder:32b",
			},
			"ollama-remote": {
				BaseURL: "http://127.0.0.1:11434/v1", // user should edit host
				APIKey:  "ollama",
				Model:   "qwen2.5-coder:32b",
			},
			// SGLang OpenAI-compatible server (default port 30000). Model id is usually
			// the served-model-name (often the GGUF basename). Prefer dense GGUF weights.
			"sglang": {
				BaseURL: "http://127.0.0.1:30000/v1",
				APIKey:  "sglang",
				Model:   "qwen2.5-coder-32b-q4_k_m.gguf",
			},
			"xai": {
				BaseURL: "https://api.x.ai/v1",
				Model:   "grok-3",
			},
			"openai": {
				BaseURL: "https://api.openai.com/v1",
				Model:   "gpt-4o-mini",
			},
		},
		MCPServers: []MCPServer{
			{
				Name:    "mcp-demo",
				Enabled: false,
				URL:     "http://127.0.0.1:8080/mcp",
			},
		},
	}
}

// Path returns the default config file path.
func Path() (string, error) {
	if p := os.Getenv("AGENTERM_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agenterm", "config.toml"), nil
}

// Load reads config from disk (or creates defaults).
func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}
	cfg := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := Save(cfg, path); err != nil {
			return cfg, path, fmt.Errorf("create default config: %w", err)
		}
		return applyEnv(cfg), path, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Merge defaults for missing provider map / presets (e.g. older configs lack sglang).
	if cfg.Providers == nil {
		cfg.Providers = Default().Providers
	} else {
		for name, p := range Default().Providers {
			if _, ok := cfg.Providers[name]; !ok {
				cfg.Providers[name] = p
			}
		}
	}
	return applyEnv(cfg), path, nil
}

// Save writes config atomically-ish.
func Save(cfg Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := toml.NewEncoder(f)
	return enc.Encode(cfg)
}

// Resolve applies provider preset + env overrides into effective BaseURL/Model/APIKey.
func (c Config) Resolve() Config {
	out := c
	if c.Provider != "" && c.Provider != "custom" {
		if p, ok := c.Providers[c.Provider]; ok {
			if out.BaseURL == "" || out.BaseURL == Default().BaseURL && c.Provider != "ollama-local" {
				// Prefer explicit top-level fields; fill from provider when empty-ish
			}
			if p.BaseURL != "" && (c.BaseURL == "" || providerOwns(c)) {
				out.BaseURL = p.BaseURL
			}
			if p.Model != "" && (c.Model == "" || providerOwns(c)) {
				// Only override model from provider if user still on defaults for that provider
			}
			// Simpler rule: if provider set, provider fields fill blanks; top-level wins when non-empty after load.
			if out.BaseURL == "" {
				out.BaseURL = p.BaseURL
			}
			if out.Model == "" {
				out.Model = p.Model
			}
			if out.APIKey == "" {
				out.APIKey = p.APIKey
			}
			// When provider is selected, use its base_url/model unless top-level was customized away from default preset.
			// Practical approach used below in applyProvider.
			out = applyProvider(c, p)
		}
	}
	if out.BaseURL == "" {
		out.BaseURL = "http://127.0.0.1:11434/v1"
	}
	out.BaseURL = strings.TrimRight(out.BaseURL, "/")
	if out.Model == "" {
		out.Model = "qwen2.5-coder:32b"
	}
	if out.APIKey == "" {
		out.APIKey = "ollama"
	}
	return out
}

func providerOwns(c Config) bool {
	return true
}

func applyProvider(c Config, p Provider) Config {
	out := c
	// Top-level config always wins if set; provider fills when loading defaults.
	// Users set provider = "ollama-remote" and edit providers.ollama-remote.base_url.
	if p.BaseURL != "" {
		// Use provider base when provider is named (typical)
		out.BaseURL = p.BaseURL
	}
	if p.Model != "" {
		// Prefer top-level model if user set it and provider model is just default
		if c.Model != "" {
			out.Model = c.Model
		} else {
			out.Model = p.Model
		}
	}
	if p.APIKey != "" && c.APIKey == "" {
		out.APIKey = p.APIKey
	} else if p.APIKey != "" && (c.APIKey == "ollama" || c.APIKey == "") {
		out.APIKey = p.APIKey
	}
	// If top-level base_url was explicitly different and provider is custom-ish, keep top-level.
	// For named providers we intentionally use provider.base_url so remote Ollama/SGLang works:
	// set [providers.ollama-remote] base_url = "http://gpu-box:11434/v1"
	// set [providers.sglang] base_url = "http://127.0.0.1:30000/v1"
	if c.Provider != "" && c.Provider != "custom" && p.BaseURL != "" {
		out.BaseURL = p.BaseURL
	}
	if c.Provider == "custom" && c.BaseURL != "" {
		out.BaseURL = c.BaseURL
	}
	// Always allow top-level model override
	if c.Model != "" {
		out.Model = c.Model
	}
	return out
}

func applyEnv(c Config) Config {
	if v := os.Getenv("BOLT_WORKSPACE"); v != "" {
		c.Workspace = v
	}
	if v := os.Getenv("AGENTERM_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := os.Getenv("AGENTERM_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("AGENTERM_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := firstEnv("AGENTERM_API_KEY", "OLLAMA_API_KEY", "XAI_API_KEY", "OPENAI_API_KEY"); v != "" {
		c.APIKey = v
	}
	// AGENTERM_ENABLE_TOOLS=0|false|off disables tools; 1|true|on enables.
	if v := strings.TrimSpace(os.Getenv("AGENTERM_ENABLE_TOOLS")); v != "" {
		switch strings.ToLower(v) {
		case "0", "false", "no", "off":
			c.EnableTools = false
		case "1", "true", "yes", "on":
			c.EnableTools = true
		}
	}
	// AGENTERM_ENABLE_SHELL=0|false|off disables run_shell; 1|true|on enables.
	if v := strings.TrimSpace(os.Getenv("AGENTERM_ENABLE_SHELL")); v != "" {
		switch strings.ToLower(v) {
		case "0", "false", "no", "off":
			c.EnableShell = false
		case "1", "true", "yes", "on":
			c.EnableShell = true
		}
	}
	return c
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Effective returns resolved runtime settings.
func (c Config) Effective() Config {
	out := applyEnv(c).Resolve()
	if out.PermissionMode == "" {
		out.PermissionMode = "allow"
	}
	if v := os.Getenv("BOLT_PERMISSION_MODE"); v != "" {
		out.PermissionMode = strings.ToLower(v)
	}
	if v := os.Getenv("BOLT_VISION"); v != "" {
		out.VisionEnabled = strings.EqualFold(v, "1") || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	}
	if strings.TrimSpace(out.Workspace) == "" {
		out.Workspace, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(out.Workspace); err == nil {
		out.Workspace = abs
	}
	return out
}

// Summary is a one-line status for the TUI header.
func (c Config) Summary() string {
	e := c.Effective()
	return fmt.Sprintf("%s · %s · %s", e.Provider, e.Model, e.BaseURL)
}
