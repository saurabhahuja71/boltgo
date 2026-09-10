package tui

import (
	"strings"
	"testing"
)

func TestAbsoluteNestedPathIsAgentPromptNotSlashCommand(t *testing.T) {
	m := testModel(t)
	updated, _ := m.handleSubmit("/scratch/sauahuja/gobin/goprojects/src/github.com/user/dboper/covered_call_bot")
	got := updated.(model)
	for _, line := range got.lines {
		if strings.Contains(line.text, "unknown command") {
			t.Fatalf("absolute path was routed as slash command: %#v", got.lines)
		}
	}
	if !got.busy {
		t.Fatal("absolute path was not sent to agent")
	}
}

func TestShortUnknownSlashCommandStillReportsError(t *testing.T) {
	m := testModel(t)
	updated, _ := m.handleSubmit("/not-a-command")
	got := updated.(model)
	if len(got.lines) == 0 || !strings.Contains(got.lines[len(got.lines)-1].text, "unknown command /not-a-command") {
		t.Fatalf("unknown slash command behavior changed: %#v", got.lines)
	}
}
