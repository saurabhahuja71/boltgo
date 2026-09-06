package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saurabhahuja71/agenterm/internal/agent"
	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	mcpclient "github.com/saurabhahuja71/agenterm/internal/mcp"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
	"github.com/saurabhahuja71/agenterm/internal/tui"
	"github.com/saurabhahuja71/agenterm/internal/upgrade"
	"github.com/spf13/cobra"
)

var (
	// Release builds override this with -X main.version. Keep local/source
	// builds aligned with the current published Bolt baseline as well.
	version        = "1.1.17"
	upgradeTimeout = 10 * time.Minute
	flagProvider   string
	flagModel      string
	flagBaseURL    string
	flagAPIKey     string
	flagConfig     string
	flagNoMCP      bool
	flagNoTools    bool
	flagShell      bool
	flagNoShell    bool
	flagPing       bool
	flagResume     bool
	flagNoResume   bool
	flagWorkspace  string
)

func main() {
	root := &cobra.Command{
		Use:     launcherName(),
		Aliases: []string{"agenterm"},
		Short:   "Bolt terminal coding agent (Ollama / SGLang / OpenAI-compatible + MCP)",
		Long:    "Bolt is a Grok-style terminal coding agent. Point it at local or remote Ollama, SGLang, xAI, OpenAI, or any OpenAI-compatible server.",
		Version: version,
		RunE:    runTUI,
	}
	// Persistent flags are shared by the interactive TUI and headless exec.
	// This keeps workspace/session/provider behavior identical for both paths.
	root.PersistentFlags().StringVar(&flagProvider, "provider", "", "provider preset: ollama-local | ollama-remote | sglang | xai | openai | custom")
	root.PersistentFlags().StringVarP(&flagModel, "model", "m", "", "model id (e.g. qwen2.5-coder:32b, qwen2.5-coder-32b-q4_k_m.gguf, grok-3)")
	root.PersistentFlags().StringVar(&flagBaseURL, "base-url", "", "OpenAI-compatible API base (e.g. http://127.0.0.1:11434/v1 or http://127.0.0.1:30000/v1)")
	root.PersistentFlags().StringVar(&flagAPIKey, "api-key", "", "API key (optional for Ollama/SGLang)")
	root.PersistentFlags().StringVar(&flagConfig, "config", "", "path to config.toml (default ~/.agenterm/config.toml)")
	root.PersistentFlags().BoolVar(&flagNoMCP, "no-mcp", false, "do not connect MCP servers from config")
	root.PersistentFlags().BoolVar(&flagNoTools, "no-tools", false, "disable function/tool calling for this session (faster chat)")
	root.PersistentFlags().BoolVar(&flagShell, "shell", false, "force-enable run_shell (bash/curl/wget/scripts)")
	root.PersistentFlags().BoolVar(&flagNoShell, "no-shell", false, "disable run_shell for this session")
	root.PersistentFlags().BoolVar(&flagPing, "ping", false, "check LLM endpoint and exit")
	root.PersistentFlags().BoolVar(&flagResume, "resume", false, "explicitly resume the workspace session")
	root.PersistentFlags().BoolVar(&flagNoResume, "no-resume", false, "explicitly start a fresh conversation")
	root.PersistentFlags().StringVarP(&flagWorkspace, "workspace", "p", "", "user workspace for all filesystem and shell tools (default: current directory)")

	var initForce bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write default config to ~/.agenterm/config.toml",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			if flagConfig != "" {
				path = flagConfig
			}
			if !initForce {
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("%s already exists (use: agenterm init --force)", path)
				}
			}
			cfg := config.Default()
			if err := config.Save(cfg, path); err != nil {
				return err
			}
			fmt.Println("wrote", path)
			fmt.Println("tips:")
			fmt.Println("  • Ollama default: http://127.0.0.1:11434/v1 (SSH tunnel OK)")
			fmt.Println("  • SGLang:        bolt --provider sglang  (http://127.0.0.1:30000/v1)")
			fmt.Println("  • Fast chat:     bolt --no-tools")
			fmt.Println("  • Ping:          bolt --ping")
			return nil
		},
	}
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite existing config with defaults")
	root.AddCommand(initCmd)

	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Bolt to the latest GitHub release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade()
		},
	}
	root.AddCommand(upgradeCmd)

	execCmd := &cobra.Command{
		Use:   "exec <prompt>",
		Short: "Run one prompt headlessly with tools enabled",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHeadless(args[0])
		},
	}
	root.AddCommand(execCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func runUpgrade() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find Bolt executable: %w", err)
	}
	repository := envOr("BOLT_UPGRADE_REPOSITORY", upgrade.DefaultRepository)
	client := upgrade.Client{
		Repository: repository,
		APIBaseURL: os.Getenv("BOLT_UPGRADE_API_URL"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradeTimeout)
	defer cancel()
	result, err := client.Upgrade(ctx, executable, version, os.Stderr)
	if err != nil {
		return fmt.Errorf("bolt upgrade failed: %w", err)
	}
	if !result.Updated {
		fmt.Fprintf(os.Stdout, "Bolt %s is already up to date (latest: %s)\n", version, result.LatestVersion)
	}
	return nil
}

func launcherName() string {
	base := strings.ToLower(filepath.Base(os.Args[0]))
	if base == "bolt-s1" || base == "bolt-s2" || base == "bolt-s3" {
		return base
	}
	return "bolt"
}

func applyLauncherPreset(cfg *config.Config, launcher string) {
	switch launcher {
	case "bolt-s1":
		cfg.Provider = "custom"
		cfg.BaseURL = envOr("BOLT_S1_BASE_URL", "http://127.0.0.1:11435/v1")
		cfg.APIKey = envOr("BOLT_S1_API_KEY", "ollama")
		cfg.Model = envOr("BOLT_S1_MODEL", "qwen3-coder:latest")
	case "bolt-s2":
		cfg.Provider = "custom"
		cfg.BaseURL = envOr("BOLT_S2_BASE_URL", envOr("SGLANG_HOST", "http://127.0.0.1:30002")+"/v1")
		cfg.APIKey = envOr("BOLT_S2_API_KEY", "sglang")
		cfg.Model = envOr("BOLT_S2_MODEL", envOr("SGLANG_DEFAULT_MODEL", "Darwin-9B-Opus"))
		cfg.PermissionMode = "allow"
	case "bolt-s3":
		cfg.Provider = "custom"
		cfg.BaseURL = envOr("BOLT_S3_BASE_URL", envOr("SGLANG3_BASE_URL", "http://127.0.0.1:30004")+"/v1")
		cfg.APIKey = envOr("BOLT_S3_API_KEY", "sglang")
		cfg.Model = envOr("BOLT_S3_MODEL", envOr("SGLANG3_MODEL", "/sglang-data/models/gpt-oss-120b"))
		cfg.PermissionMode = "allow"
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func resumeRequested() bool {
	if flagResume {
		return true
	}
	if flagNoResume {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("BOLT_RESUME")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func runHeadless(prompt string) error {
	if flagConfig != "" {
		_ = os.Setenv("AGENTERM_CONFIG", flagConfig)
	}

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	applyLauncherPreset(&cfg, launcherName())
	if flagProvider != "" {
		cfg.Provider = flagProvider
	}
	if flagModel != "" {
		cfg.Model = flagModel
	}
	if flagBaseURL != "" {
		cfg.BaseURL = flagBaseURL
		cfg.Provider = "custom"
	}
	if flagAPIKey != "" {
		cfg.APIKey = flagAPIKey
	}
	if flagWorkspace != "" {
		cfg.Workspace = flagWorkspace
	}
	if flagShell {
		cfg.EnableShell = true
	}
	if flagNoShell {
		cfg.EnableShell = false
	}
	if flagNoTools {
		cfg.EnableTools = false
	}
	if flagResume && flagNoResume {
		return fmt.Errorf("--resume and --no-resume are mutually exclusive")
	}
	eff := cfg.Effective()
	// Bolt's headless exec mode has no approval surface; preserve boltpy's
	// explicit exec behavior while the interactive TUI remains ASK by default.
	if os.Getenv("BOLT_PERMISSION_MODE") == "" {
		eff.PermissionMode = "allow"
	}
	client := llm.New(eff.BaseURL, eff.APIKey)
	reg := tools.DefaultBuiltinsOpts(tools.BuiltinOpts{
		EnableShell: eff.EnableShell,
		TestCommand: eff.TestCommand,
		Workspace:   eff.Workspace,
	})

	var mcpMgr *mcpclient.Manager
	if !flagNoMCP {
		mcpMgr = mcpclient.NewManager()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := mcpMgr.ConnectAll(ctx, eff.MCPServers); err != nil {
			fmt.Fprintf(os.Stderr, "mcp: %v\n", err)
		}
		cancel()
		mcpMgr.RegisterOnto(reg)
		defer mcpMgr.Close()
	}

	ag := agent.New(eff, client, reg)
	saveSession := true
	sessionPath, err := agent.WorkspaceSessionPath(eff.Workspace, "latest")
	if err != nil {
		return err
	}
	if resumeRequested() {
		if err := ag.LoadSession(sessionPath); err != nil {
			fmt.Fprintf(os.Stderr, "session resume: %v\n", err)
			saveSession = false
		} else {
			fmt.Fprintf(os.Stderr, "resumed session %s\n", sessionPath)
		}
	}
	ctx := context.Background()
	err = ag.RunUserMessage(ctx, prompt, func(event agent.Event) {
		switch event.Kind {
		case agent.EventToken:
			fmt.Print(event.Text)
		case agent.EventToolStart:
			fmt.Fprintf(os.Stderr, "\n[tool %s] %s\n", event.Tool, event.Text)
		case agent.EventToolEnd:
			if event.ToolOut != "" {
				fmt.Fprintf(os.Stdout, "\n[tool %s output]\n%s\n", event.Tool, event.ToolOut)
			}
		case agent.EventStatus:
			fmt.Fprintf(os.Stderr, "[%s]\n", event.Text)
		case agent.EventError:
			fmt.Fprintf(os.Stderr, "\n[error] %s\n", event.Text)
		case agent.EventPermission:
			if event.Decision != nil {
				event.Decision <- permissions.Deny
			}
		}
	})
	if err == nil {
		if saveSession {
			_, _ = ag.SaveSessionPath(sessionPath)
		}
	}
	return err
}

func runTUI(cmd *cobra.Command, args []string) error {
	if flagConfig != "" {
		_ = os.Setenv("AGENTERM_CONFIG", flagConfig)
	}

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}
	applyLauncherPreset(&cfg, launcherName())

	// CLI overrides
	if flagProvider != "" {
		cfg.Provider = flagProvider
	}
	if flagModel != "" {
		cfg.Model = flagModel
	}
	if flagBaseURL != "" {
		cfg.BaseURL = flagBaseURL
		cfg.Provider = "custom"
	}
	if flagAPIKey != "" {
		cfg.APIKey = flagAPIKey
	}
	if flagWorkspace != "" {
		cfg.Workspace = flagWorkspace
	}
	if flagShell {
		cfg.EnableShell = true
	}
	if flagNoShell {
		cfg.EnableShell = false
	}
	if flagNoTools {
		cfg.EnableTools = false
	}
	if flagResume && flagNoResume {
		return fmt.Errorf("--resume and --no-resume are mutually exclusive")
	}

	eff := cfg.Effective()
	// CLI flags win over env/config for shell.
	if flagShell {
		eff.EnableShell = true
	}
	if flagNoShell {
		eff.EnableShell = false
	}
	client := llm.New(eff.BaseURL, eff.APIKey)

	if flagPing {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Ping(ctx); err != nil {
			return fmt.Errorf("ping %s: %w", eff.BaseURL, err)
		}
		fmt.Printf("ok  provider=%s model=%s url=%s\n", eff.Provider, eff.Model, eff.BaseURL)
		return nil
	}

	reg := tools.DefaultBuiltinsOpts(tools.BuiltinOpts{
		EnableShell: eff.EnableShell,
		TestCommand: eff.TestCommand,
		Workspace:   eff.Workspace,
	})

	var mcpMgr *mcpclient.Manager
	if !flagNoMCP {
		mcpMgr = mcpclient.NewManager()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := mcpMgr.ConnectAll(ctx, eff.MCPServers); err != nil {
			// non-fatal: show in TUI via stderr
			fmt.Fprintf(os.Stderr, "mcp: %v\n", err)
		}
		cancel()
		mcpMgr.RegisterOnto(reg)
		defer mcpMgr.Close()
	}

	ag := agent.New(eff, client, reg)

	// Bolt sessions are opt-in: a normal launch is always a genuinely fresh history.
	resume := resumeRequested()
	saveSession := true
	sessionPath, err := agent.WorkspaceSessionPath(eff.Workspace, "latest")
	if err != nil {
		return err
	}
	if resume {
		if err := ag.LoadSession(sessionPath); err != nil {
			fmt.Fprintf(os.Stderr, "session resume: %v\n", err)
			saveSession = false
		} else {
			fmt.Fprintf(os.Stderr, "  resumed session %s\n", sessionPath)
		}
	}

	fmt.Fprintf(os.Stderr, "agenterm %s  config=%s\n", version, path)
	fmt.Fprintf(os.Stderr, "  %s\n", eff.Summary())

	return tui.Run(tui.Deps{
		Title:       "Bolt",
		Summary:     eff.Summary(),
		Agent:       ag,
		Workspace:   eff.Workspace,
		SessionPath: sessionPath,
		SaveSession: saveSession,
	})
}
