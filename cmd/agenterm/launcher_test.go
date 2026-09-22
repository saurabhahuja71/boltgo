package main

import (
	"os"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
)

func TestLauncherPresetsShareOneRuntime(t *testing.T) {
	for _, name := range []string{"bolt", "bolt-s1", "bolt-s2", "bolt-s3", "bolt-s4", "bolt-s5", "bolt-s6", "bolt-s7", "bolt-s8"} {
		old := os.Args[0]
		os.Args[0] = name
		if got := launcherName(); got != name {
			t.Fatalf("%s -> %s", name, got)
		}
		os.Args[0] = old
		cfg := config.Default()
		applyLauncherPreset(&cfg, name)
		if cfg.BaseURL == "" || cfg.Model == "" {
			t.Fatalf("%s incomplete preset: %#v", name, cfg)
		}
		if (name == "bolt-s1" || name == "bolt-s2" || name == "bolt-s3") && cfg.PermissionMode != "allow" {
			t.Fatalf("%s permission mode = %q, want allow", name, cfg.PermissionMode)
		}
	}
}

func TestLauncherPreservesConfiguredModelAndEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Model, cfg.BaseURL, cfg.Provider = "configured-model", "http://configured.example/v1", "custom"
	for _, name := range []string{"bolt-s1", "bolt-s2", "bolt-s3", "bolt-s4", "bolt-s5", "bolt-s6", "bolt-s7", "bolt-s8"} {
		got := cfg
		applyLauncherEnvironment(&got, name)
		applyLauncherPreset(&got, name)
		if got.Model != cfg.Model || got.BaseURL != cfg.BaseURL {
			t.Fatalf("%s replaced configured values: got model=%q base=%q", name, got.Model, got.BaseURL)
		}
	}
}

func TestLauncherEnvironmentOverridesConfiguredValues(t *testing.T) {
	t.Setenv("BOLT_S1_MODEL", "env-model")
	t.Setenv("BOLT_S1_BASE_URL", "http://env.example/v1")
	cfg := config.Default()
	cfg.Model, cfg.BaseURL = "configured-model", "http://configured.example/v1"
	applyLauncherEnvironment(&cfg, "bolt-s1")
	if cfg.Model != "env-model" || cfg.BaseURL != "http://env.example/v1" || cfg.Provider != "custom" {
		t.Fatalf("environment override failed: %#v", cfg)
	}
}

func TestBoltS1PolicyDefaultAndExplicitPolicy(t *testing.T) {
	cfg := config.Default()
	applyLauncherPreset(&cfg, "bolt-s1")
	if cfg.Model != "Qwen3.6-27B-Q3_K_M.gguf" {
		t.Fatalf("default bolt-s1 model = %q, want Qwen3.6-27B-Q3_K_M.gguf", cfg.Model)
	}
	if cfg.PermissionMode != "allow" {
		t.Fatalf("default bolt-s1 policy = %q, want allow", cfg.PermissionMode)
	}
	cfg = config.Default()
	cfg.PermissionMode, cfg.PermissionModeConfigured = "ask", true
	applyLauncherPreset(&cfg, "bolt-s1")
	if cfg.PermissionMode != "ask" {
		t.Fatalf("explicit bolt-s1 policy was replaced: %q", cfg.PermissionMode)
	}
}

func TestBoltS1PreservesExplicitModelAndEnvironmentOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Model, cfg.ModelConfigured = "configured-model", true
	applyLauncherPreset(&cfg, "bolt-s1")
	if cfg.Model != "configured-model" {
		t.Fatalf("explicit bolt-s1 model was replaced: %q", cfg.Model)
	}

	t.Setenv("BOLT_S1_MODEL", "environment-model")
	cfg = config.Default()
	applyLauncherEnvironment(&cfg, "bolt-s1")
	applyLauncherPreset(&cfg, "bolt-s1")
	if cfg.Model != "environment-model" {
		t.Fatalf("BOLT_S1_MODEL was replaced: %q", cfg.Model)
	}
}

func TestResumeRequestedPrecedence(t *testing.T) {
	oldResume, oldNoResume := flagResume, flagNoResume
	oldArg := os.Args[0]
	t.Cleanup(func() { flagResume, flagNoResume, os.Args[0] = oldResume, oldNoResume, oldArg })
	os.Args[0] = "bolt"
	flagResume, flagNoResume = "", false
	if resumeRequested() {
		t.Fatal("normal Bolt launch should start a fresh session by default")
	}
	flagResume = "latest"
	if !resumeRequested() {
		t.Fatal("--resume latest should request resume")
	}
	flagNoResume = true
	if resumeRequested() {
		t.Fatal("--no-resume should override BOLT_RESUME")
	}
	flagNoResume = false
	flagResume = "latest"
	if !resumeRequested() {
		t.Fatal("--resume latest should override an empty default")
	}
}

func TestBoltS3StartsFreshUnlessExplicitResume(t *testing.T) {
	oldResume, oldNoResume, oldArg := flagResume, flagNoResume, os.Args[0]
	t.Cleanup(func() { flagResume, flagNoResume, os.Args[0] = oldResume, oldNoResume, oldArg })
	os.Args[0] = "bolt-s3"
	flagResume, flagNoResume = "", false
	t.Setenv("BOLT_RESUME", "1")
	if resumeRequested() {
		t.Fatal("bolt-s3 must ignore implicit BOLT_RESUME")
	}
	flagResume = "latest"
	if !resumeRequested() {
		t.Fatal("explicit --resume latest must still resume bolt-s3")
	}
}
