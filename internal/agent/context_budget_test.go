package agent

import (
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

func TestModelHistoryForRequestCompactsOlderConversation(t *testing.T) {
	a := &Agent{History: []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "old request"},
		{Role: llm.RoleAssistant, Content: strings.Repeat("old raw answer ", 4_000)},
		{Role: llm.RoleUser, Content: "inspect the workflow URL"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "fetch", Arguments: `{"url":"https://example.test"}`}}}},
		{Role: llm.RoleTool, ToolCallID: "call-1", Name: "fetch", Content: strings.Repeat("large response ", 2_000)},
	}}

	got := a.modelHistoryForRequest()
	if messageChars(got) > modelHistoryBudget {
		t.Fatalf("model history=%d chars, budget=%d", messageChars(got), modelHistoryBudget)
	}
	joined := ""
	for _, message := range got {
		joined += message.Content
	}
	if strings.Contains(joined, "old raw answer") {
		t.Fatal("old raw assistant output survived compaction")
	}
	if !strings.Contains(joined, "inspect the workflow URL") || !strings.Contains(joined, "large response") {
		t.Fatalf("current request/tool evidence was lost: %+v", got)
	}
	if got[0].Role != llm.RoleSystem {
		t.Fatalf("system message was not preserved: %+v", got)
	}
}

func TestModelHistoryForRequestLeavesSmallConversationUnchanged(t *testing.T) {
	a := &Agent{History: []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "hello"},
		{Role: llm.RoleAssistant, Content: "hi"},
	}}

	got := a.modelHistoryForRequest()
	if len(got) != len(a.History) || got[1].Content != "hello" || got[2].Content != "hi" {
		t.Fatalf("small conversation changed: %+v", got)
	}
}
