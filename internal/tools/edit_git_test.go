package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitArgsFlexible(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`{"args":["checkout","-b","seo-readme"]}`, []string{"checkout", "-b", "seo-readme"}},
		{`{"args":"checkout -b seo-readme"}`, []string{"checkout", "-b", "seo-readme"}},
		{`{"args":"[\"checkout\",\"-b\",\"seo-readme\"]"}`, []string{"checkout", "-b", "seo-readme"}},
		{`{"command":"git status -sb"}`, []string{"status", "-sb"}},
		{`["checkout","-b","x"]`, []string{"checkout", "-b", "x"}},
	}
	for _, c := range cases {
		got, err := parseGitArgsJSON(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Fatalf("%s => %v want %v", c.in, got, c.want)
		}
	}
}

func TestStrReplaceAndGitAllowlist(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	path := filepath.Join(dir, "README.md")
	if err := os.WriteFile(path, []byte("# Hello\n\nold line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := DefaultBuiltins(false)
	out, err := r.Run(context.Background(), "str_replace", `{"path":"README.md","old_string":"old line","new_string":"new line"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated") {
		t.Fatalf("out=%q", out)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new line") || strings.Contains(string(data), "old line") {
		t.Fatalf("file not updated: %s", data)
	}

	if err := validateGitArgs([]string{"status"}); err != nil {
		t.Fatal(err)
	}
	if err := validateGitArgs([]string{"rm", "-rf", "/"}); err == nil {
		t.Fatal("expected reject dangerous subcommand")
	}
	if err := validateGitArgs([]string{"push", "--force"}); err == nil {
		t.Fatal("expected reject force push without env")
	}
}

func TestIsActionRequest_viaAgentPackage(t *testing.T) {
	// kept in agent tests; placeholder so tools package stays focused
}

func TestGitAddDotBlocked(t *testing.T) {
	if err := validateGitArgs([]string{"add", "."}); err == nil {
		t.Fatal("git add . should be blocked")
	}
	if err := validateGitArgs([]string{"add", ".bolt/sessions/latest.json"}); err == nil {
		t.Fatal("git add .bolt path should be blocked")
	}
	if err := validateGitArgs([]string{"add", "underlyings.txt"}); err != nil {
		t.Fatalf("specific git add should be allowed: %v", err)
	}
}
