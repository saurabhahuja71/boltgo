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

func TestModelHistoryCompactionRetainsAllAuthoritativeObservations(t *testing.T) {
	a := &Agent{
		History: []llm.Message{
			{Role: llm.RoleSystem, Content: "system"},
			{Role: llm.RoleUser, Content: "investigate the repository"},
			{Role: llm.RoleAssistant, Content: strings.Repeat("intermediate model prose ", 500)},
			{Role: llm.RoleTool, ToolCallID: "call-a", Name: "read_file", Content: strings.Repeat("raw A ", 2_000)},
			{Role: llm.RoleAssistant, Content: strings.Repeat("more model prose ", 500)},
			{Role: llm.RoleTool, ToolCallID: "call-b", Name: "read_file", Content: strings.Repeat("raw B ", 2_000)},
		},
		RunState: AgentRunState{
			Observations: []Observation{
				{Tool: "read_file", Summary: "first fact: position state is stale", Success: true},
				{Tool: "read_file", Summary: "second fact: runtime log contradicts the first explanation", Success: true},
			},
			ToolCalls: []ToolCallRecord{
				{Name: "read_file", Arguments: `{"path":"state.json"}`},
				{Name: "read_file", Arguments: `{"path":"runtime.log"}`},
			},
		},
	}

	got := a.modelHistoryForRequest()
	joined := ""
	for _, message := range got {
		joined += message.Content + "\n"
	}
	if !strings.Contains(joined, "first fact: position state is stale") || !strings.Contains(joined, "second fact: runtime log contradicts") {
		t.Fatalf("compaction lost authoritative observations: %+v", got)
	}
}

func TestModelHistoryCompactionDoesNotLeaveOrphanToolResults(t *testing.T) {
	a := &Agent{History: []llm.Message{
		{Role: llm.RoleSystem, Content: "system"},
		{Role: llm.RoleUser, Content: "inspect the repository"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-a", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{}`}}}},
		{Role: llm.RoleTool, ToolCallID: "call-a", Name: "read_file", Content: "first result"},
		{Role: llm.RoleAssistant, Content: strings.Repeat("large intermediate answer ", 2_000), ToolCalls: []llm.ToolCall{{ID: "call-b", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{}`}}}},
		{Role: llm.RoleTool, ToolCallID: "call-b", Name: "read_file", Content: "second result"},
	}}

	got := a.modelHistoryForRequest()
	assistantCalls := map[string]bool{}
	for _, message := range got {
		if message.Role == llm.RoleAssistant {
			for _, call := range message.ToolCalls {
				assistantCalls[call.ID] = true
			}
		}
		if message.Role == llm.RoleTool && !assistantCalls[message.ToolCallID] {
			t.Fatalf("orphan tool result survived compaction: %+v", got)
		}
	}
}
