package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	version              = "1.1.38"
	upgradeTimeout       = 10 * time.Minute
	flagProvider         string
	flagModel            string
	flagBaseURL          string
	flagAPIKey           string
	flagConfig           string
	flagNoMCP            bool
	flagNoTools          bool
	flagShell            bool
	flagNoShell          bool
	flagPing             bool
	flagResume           string
	flagNoResume         bool
	flagWorkspace        string
	flagGoalGraph        bool
	flagCompatibility    bool
	flagDaily            bool
	flagInferenceProfile string
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
	root.PersistentFlags().StringVar(&flagResume, "resume", "", "resume a workspace session by ID (for example: latest)")
	root.PersistentFlags().BoolVar(&flagNoResume, "no-resume", false, "explicitly start a fresh conversation")
	root.PersistentFlags().StringVar(&flagInferenceProfile, "inference-profile", "", "opt-in inference profile (e.g. local-coding-reproducible)")
	root.PersistentFlags().StringVarP(&flagWorkspace, "workspace", "p", "", "user workspace for all filesystem and shell tools (default: current directory)")
	root.PersistentFlags().BoolVar(&flagDaily, "daily", false, "opt in to supervised daily coding mode")

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
	execCmd.Flags().BoolVar(&flagGoalGraph, "goal-graph", false, "enable opt-in Goal Graph planning and scheduling")
	execCmd.Flags().BoolVar(&flagCompatibility, "compatibility", false, "enable legacy-compatible execution with deterministic verification guidance")
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
	if strings.HasPrefix(base, "bolt-s") {
		if n, err := strconv.Atoi(strings.TrimPrefix(base, "bolt-s")); err == nil && n >= 1 && n <= 8 {
			return base
		}
	}
	return "bolt"
}

func applyLauncherPreset(cfg *config.Config, launcher string) {
	switch launcher {
	case "bolt-s1":
		// S1 has its own backend/model contract. The shared config may name a
		// model for the other launchers, but must not silently change S1.
		if os.Getenv("BOLT_S1_MODEL") == "" {
			cfg.Model = "Qwen3.6-27B-Q3_K_M.gguf"
		}
		if !cfg.PermissionModeConfigured && os.Getenv("BOLT_PERMISSION_MODE") == "" {
			cfg.PermissionMode = "allow"
		}
	case "bolt-s2":
		if !cfg.PermissionModeConfigured && os.Getenv("BOLT_PERMISSION_MODE") == "" {
			cfg.PermissionMode = "allow"
		}
	case "bolt-s3":
		if !cfg.PermissionModeConfigured && os.Getenv("BOLT_PERMISSION_MODE") == "" {
			cfg.PermissionMode = "allow"
		}
		// S3's current SGLang backend needs visible answers instead of
		// reasoning-only responses; this is behavioral, not model selection.
		cfg.DisableThinking = true
	}
}

func applyLauncherEnvironment(cfg *config.Config, launcher string) {
	prefix := strings.ToUpper(strings.TrimPrefix(launcher, "bolt-"))
	if !strings.HasPrefix(prefix, "S") {
		return
	}
	if v := os.Getenv("BOLT_" + prefix + "_BASE_URL"); v != "" {
		cfg.BaseURL, cfg.Provider = v, "custom"
	}
	if v := os.Getenv("BOLT_" + prefix + "_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("BOLT_" + prefix + "_API_KEY"); v != "" {
		cfg.APIKey = v
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func resumeRequested() bool {
	return strings.TrimSpace(flagResume) != "" && !flagNoResume
}

func runHeadless(prompt string) error {
	if flagDaily && flagGoalGraph {
		return fmt.Errorf("--daily and --goal-graph are mutually exclusive; daily mode uses compatibility execution")
	}
	if flagGoalGraph && flagCompatibility {
		return fmt.Errorf("--goal-graph and --compatibility are mutually exclusive")
	}
	if flagConfig != "" {
		_ = os.Setenv("AGENTERM_CONFIG", flagConfig)
	}

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	launcher := launcherName()
	applyLauncherEnvironment(&cfg, launcher)
	applyLauncherPreset(&cfg, launcher)
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
	if flagInferenceProfile != "" {
		cfg.InferenceProfile = flagInferenceProfile
	}
	if flagDaily {
		flagCompatibility = true
		if isOllamaConfig(cfg.Effective()) {
			cfg.InferenceProfile = "local-coding-reproducible"
		}
	}
	if strings.TrimSpace(flagResume) != "" && flagNoResume {
		return fmt.Errorf("--resume and --no-resume are mutually exclusive")
	}
	eff := cfg.Effective()
	sampling, err := llm.ResolveSamplingProfile(eff.InferenceProfile)
	if err != nil {
		return err
	}
	if err := llm.ValidateSamplingProfile(context.Background(), eff.BaseURL, eff.Model, eff.InferenceProfile); err != nil {
		return err
	}
	if eff.InferenceProfile != "" {
		profileJSON, _ := json.Marshal(sampling)
		fmt.Fprintf(os.Stderr, "inference profile: %s options=%s\n", eff.InferenceProfile, profileJSON)
	}
	// Bolt's headless exec mode has no approval surface; preserve boltpy's
	// explicit exec behavior while the interactive TUI remains ASK by default.
	if os.Getenv("BOLT_PERMISSION_MODE") == "" && !flagDaily {
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
	ag.SetInferenceProfile(eff.InferenceProfile, sampling)
	if flagDaily {
		ag.EnableDailyMode()
		fmt.Fprintf(os.Stderr, "daily mode: enabled; stage=inspect; permission=%s; workspace=%s\n", eff.PermissionMode, eff.Workspace)
	}
	if flagCompatibility {
		ag.EnableCompatibilityMode()
		fmt.Fprintln(os.Stderr, "compatibility mode: enabled; legacy execution loop with deterministic verification guidance")
	}
	if flagGoalGraph {
		proposer := agent.ConfiguredGoalGraphProposer{
			Client: client, Model: eff.Model, Temperature: eff.Temperature, MaxTokens: eff.MaxTokens,
		}
		graph, activation, err := agent.ConstructGoalGraphWithFallback(context.Background(), prompt, proposer)
		if err != nil {
			return fmt.Errorf("goal graph activation failed: %w", err)
		}
		if err := ag.EnableGoalGraph(graph); err != nil {
			return fmt.Errorf("enable goal graph: %w", err)
		}
		fmt.Fprintf(os.Stderr, "goal graph: enabled; source=%s; nodes=%d; required=%d; requirements=%s\n", activation.Source, len(graph.Nodes), len(graph.RequiredNodeIDs), strings.Join(activation.RequirementIDs, ","))
		if activation.ProposalRejection != "" {
			fmt.Fprintf(os.Stderr, "goal graph: rejected model proposal: %s\n", activation.ProposalRejection)
		}
	}
	saveSession := true
	sessionPath, err := agent.WorkspaceSessionPath(eff.Workspace, "latest")
	if err != nil {
		return err
	}
	if resumeRequested() {
		resumePath, pathErr := agent.WorkspaceSessionPath(eff.Workspace, strings.TrimSpace(flagResume))
		if pathErr != nil {
			return pathErr
		}
		if err := ag.LoadSession(resumePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "session resume: no previous session at %s\n", resumePath)
			} else {
				fmt.Fprintf(os.Stderr, "session resume: %v\n", err)
				saveSession = false
			}
		} else {
			fmt.Fprintf(os.Stderr, "resumed session %s\n", resumePath)
		}
	}
	if flagDaily && saveSession {
		ag.SetDailyPersistence(func() error {
			_, err := ag.SaveSessionPath(sessionPath)
			return err
		})
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
			if flagDaily {
				if handoff := ag.DailyHandoff(event.Text); handoff != "" {
					fmt.Fprintf(os.Stderr, "\n[%s]\n", handoff)
				}
			}
		case agent.EventPermission:
			if event.Decision != nil {
				event.Decision <- permissions.Deny
			}
		}
	})
	if flagDaily {
		fmt.Fprintf(os.Stderr, "\n[%s]\n", ag.FactualSummary())
	}
	if saveSession {
		_, _ = ag.SaveSessionPath(sessionPath)
	}
	return err
}

func runTUI(cmd *cobra.Command, args []string) error {
	if flagConfig != "" {
		_ = os.Setenv("AGENTERM_CONFIG", flagConfig)
	}

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	launcher := launcherName()
	applyLauncherEnvironment(&cfg, launcher)
	applyLauncherPreset(&cfg, launcher)

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
	if flagInferenceProfile != "" {
		cfg.InferenceProfile = flagInferenceProfile
	}
	if flagDaily && isOllamaConfig(cfg.Effective()) {
		cfg.InferenceProfile = "local-coding-reproducible"
	}
	if strings.TrimSpace(flagResume) != "" && flagNoResume {
		return fmt.Errorf("--resume and --no-resume are mutually exclusive")
	}

	eff := cfg.Effective()
	sampling, err := llm.ResolveSamplingProfile(eff.InferenceProfile)
	if err != nil {
		return err
	}
	if err := llm.ValidateSamplingProfile(context.Background(), eff.BaseURL, eff.Model, eff.InferenceProfile); err != nil {
		return err
	}
	if eff.InferenceProfile != "" {
		profileJSON, _ := json.Marshal(sampling)
		fmt.Fprintf(os.Stderr, "inference profile: %s options=%s\n", eff.InferenceProfile, profileJSON)
	}
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
	ag.SetInferenceProfile(eff.InferenceProfile, sampling)
	if flagDaily {
		ag.EnableDailyMode()
	}

	// Bolt starts fresh by default. --resume <session-id> explicitly restores
	// a saved workspace session; --no-resume is an explicit fresh override.
	resume := resumeRequested()
	saveSession := true
	sessionPath, err := agent.WorkspaceSessionPath(eff.Workspace, "latest")
	if err != nil {
		return err
	}
	if resume {
		resumePath, pathErr := agent.WorkspaceSessionPath(eff.Workspace, strings.TrimSpace(flagResume))
		if pathErr != nil {
			return pathErr
		}
		if err := ag.LoadSession(resumePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "session resume: no previous session at %s\n", resumePath)
			} else {
				fmt.Fprintf(os.Stderr, "session resume: %v\n", err)
				saveSession = false
			}
		} else {
			fmt.Fprintf(os.Stderr, "  resumed session %s\n", resumePath)
		}
	}
	if flagDaily && saveSession {
		ag.SetDailyPersistence(func() error {
			_, err := ag.SaveSessionPath(sessionPath)
			return err
		})
	}

	err = tui.Run(tui.Deps{
		Title:       "Bolt",
		Summary:     eff.Summary(),
		Agent:       ag,
		Workspace:   eff.Workspace,
		SessionPath: sessionPath,
		SaveSession: saveSession,
		Mode:        ag.ModeName(),
	})
	if saveSession {
		if _, statErr := os.Stat(sessionPath); statErr == nil {
			fmt.Fprintf(os.Stderr, "session id: latest (resume with: bolt --resume latest)\n")
		}
	}
	return err
}

func isOllamaConfig(cfg config.Config) bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	base := strings.ToLower(strings.TrimSpace(cfg.BaseURL))
	return strings.Contains(provider, "ollama") || strings.Contains(base, "ollama") ||
		strings.Contains(base, ":11434") || strings.Contains(base, ":11435")
}
