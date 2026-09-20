package main

import (
	"os"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
)

func TestLauncherPresetsShareOneRuntime(t *testing.T) {
	for _, name := range []string{"bolt", "bolt-s1", "bolt-s2", "bolt-s3"} {
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
	}
}

func TestLauncherPresetsUseDedicatedSGLangSettings(t *testing.T) {
	t.Setenv("SGLANG_HOST", "http://127.0.0.1:30000")
	t.Setenv("SGLANG2_LOCAL_PORT", "30002")
	cfg := config.Default()
	applyLauncherPreset(&cfg, "bolt-s2")
	if cfg.BaseURL != "http://127.0.0.1:30002/v1" {
		t.Fatalf("bolt-s2 BaseURL = %q, want dedicated local port", cfg.BaseURL)
	}

	cfg = config.Default()
	applyLauncherPreset(&cfg, "bolt-s3")
	if !cfg.DisableThinking {
		t.Fatal("bolt-s3 must disable Qwen3 thinking-only responses")
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
