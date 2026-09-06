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

func TestResumeRequestedPrecedence(t *testing.T) {
	oldResume, oldNoResume := flagResume, flagNoResume
	t.Cleanup(func() { flagResume, flagNoResume = oldResume, oldNoResume })
	flagResume, flagNoResume = false, false
	t.Setenv("BOLT_RESUME", "1")
	if !resumeRequested() {
		t.Fatal("BOLT_RESUME=1 should request resume")
	}
	flagNoResume = true
	if resumeRequested() {
		t.Fatal("--no-resume should override BOLT_RESUME")
	}
	flagNoResume = false
	flagResume = true
	t.Setenv("BOLT_RESUME", "0")
	if !resumeRequested() {
		t.Fatal("--resume should override BOLT_RESUME=0")
	}
}
