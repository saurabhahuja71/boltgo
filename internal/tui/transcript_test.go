package tui

import (
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

func TestSessionChatLinesRestoresConversationRoles(t *testing.T) {
	lines := sessionChatLines([]llm.Message{
		{Role: llm.RoleSystem, Content: "private system prompt"},
		{Role: llm.RoleUser, Content: "first question"},
		{Role: llm.RoleAssistant, Content: "first answer"},
		{Role: llm.RoleTool, Name: "read_file", Content: "file output"},
	})
	if len(lines) != 3 || lines[0].role != "user" || lines[1].role != "assistant" || lines[2].role != "tool" {
		t.Fatalf("session roles were not restored: %#v", lines)
	}
}

func TestActiveTurnKeepsOneAssistantLine(t *testing.T) {
	m := model{
		lines:         []chatLine{{role: "user", text: "check the cluster"}},
		stream:        &strings.Builder{},
		busy:          true,
		turnAssistant: -1,
	}

	m.upsertThinkingPlaceholder()
	if got := countRole(m.lines, "assistant-stream"); got != 1 {
		t.Fatalf("thinking created %d assistant lines, want 1", got)
	}

	m.ensureStream().WriteString("checking")
	m.upsertStreamingAssistant("checking")
	m.lines = append(m.lines, chatLine{role: "tool", text: "ssh_execute · ok"})
	m.upsertThinkingPlaceholder()
	m.ensureStream().Reset()
	m.ensureStream().WriteString("cluster is healthy")
	m.flushStreamAsLineQuiet()

	if got := countRole(m.lines, "assistant"); got != 1 {
		t.Fatalf("final response created %d assistant lines, want 1: %#v", got, m.lines)
	}
	if got := countRole(m.lines, "assistant-stream"); got != 0 {
		t.Fatalf("thinking/stream placeholder remained as %d assistant-stream lines", got)
	}
	if !strings.Contains(m.lines[m.turnAssistant].text, "cluster is healthy") {
		t.Fatalf("final response did not replace active turn: %#v", m.lines[m.turnAssistant])
	}
}

func countRole(lines []chatLine, role string) int {
	n := 0
	for _, line := range lines {
		if line.role == role {
			n++
		}
	}
	return n
}
