package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/permissions"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

type scriptedTool struct {
	name    string
	mu      sync.Mutex
	calls   []string
	outputs []scriptedOutcome
}

type scriptedOutcome struct {
	out string
	err error
}

func (s *scriptedTool) Name() string           { return s.name }
func (s *scriptedTool) Description() string    { return "test tool" }
func (s *scriptedTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (s *scriptedTool) Run(_ context.Context, args string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, args)
	if len(s.outputs) == 0 {
		return "ok", nil
	}
	i := len(s.calls) - 1
	if i >= len(s.outputs) {
		i = len(s.outputs) - 1
	}
	return s.outputs[i].out, s.outputs[i].err
}

func toolSSE(name, args string) string {
	call := llm.ToolCall{ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
	b, _ := json.Marshal(call)
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[%s]}}]}\n\ndata: [DONE]\n\n", b)
}

func toolSSEMultiple(calls ...llm.ToolCall) string {
	b, _ := json.Marshal(calls)
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":%s}}]}\n\ndata: [DONE]\n\n", b)
}

func textSSE(text string) string {
	b, _ := json.Marshal(text)
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\ndata: [DONE]\n\n", b)
}

func testAgent(t *testing.T, replies func(int) string, reg *tools.Registry) (*Agent, *[]llm.ChatRequest, func()) {
	t.Helper()
	var mu sync.Mutex
	var requests []llm.ChatRequest
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		n++
		requests = append(requests, req)
		body := replies(n)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))

	cfg := config.Default()
	cfg.BaseURL, cfg.Model, cfg.PermissionMode = server.URL, "test", "allow"
	ag := New(cfg, llm.New(server.URL, ""), reg)
	return ag, &requests, server.Close
}

func TestAgentSimpleTaskPreservesGoalAndVerifies(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"README.md"}`)
		}
		if n == 2 {
			return textSSE("intermediate verification")
		}
		return textSSE("done")
	}, reg)
	defer closeServer()

	var tokens []string
	if err := ag.RunUserMessage(context.Background(), "inspect README.md and report", func(e Event) {
		if e.Kind == EventToken {
			tokens = append(tokens, e.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.OriginalGoal != "inspect README.md and report" || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("state=%+v", ag.RunState)
	}
	if !strings.Contains(strings.Join(tokens, ""), "done") {
		t.Fatalf("tokens=%q", tokens)
	}
	if len(*requests) != 3 || !strings.Contains((*requests)[2].Messages[len((*requests)[2].Messages)-1].Content, "verification gate") {
		t.Fatalf("expected private verification request, requests=%d", len(*requests))
	}
	if !strings.Contains((*requests)[0].Messages[len((*requests)[0].Messages)-1].Content, "acceptance_criteria") ||
		!strings.Contains((*requests)[0].Messages[len((*requests)[0].Messages)-1].Content, "inspect README.md and report") {
		t.Fatalf("control state omitted goal/acceptance criteria: %+v", (*requests)[0].Messages[len((*requests)[0].Messages)-1])
	}
}

func TestAgentStructuredToolProtocolChainsResultIntoNextCall(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	search := &scriptedTool{name: "grep"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(search)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("read_file", `{"path":"README.md"}`)
		case 2:
			return toolSSE("grep", `{"pattern":"calc","path":"README.md"}`)
		default:
			return textSSE("final response based on the tool results")
		}
	}, reg)
	defer closeServer()

	if err := ag.RunUserMessage(context.Background(), "inspect README.md and report", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 1 || len(search.calls) != 1 || len(*requests) < 3 {
		t.Fatalf("structured chain did not execute: read=%d grep=%d requests=%d", len(reader.calls), len(search.calls), len(*requests))
	}
	second := (*requests)[1]
	seenReadResult := false
	for _, msg := range second.Messages {
		if msg.Role == llm.RoleTool && msg.ToolCallID == "call-1" && strings.Contains(msg.Content, "ok") && msg.Name == "read_file" {
			seenReadResult = true
		}
	}
	if !seenReadResult {
		t.Fatalf("second request lost read tool-result association: %+v", second.Messages)
	}
	if ag.RunState.Verification != VerificationPassed {
		t.Fatalf("final response did not pass verification: %+v", ag.RunState)
	}
}

func TestAgentPreservesConfiguredOutputLimitAfterTools(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"README.md"}`)
		}
		return textSSE("verified")
	}, reg)
	defer closeServer()
	ag.Cfg.MaxTokens = 2048

	if err := ag.RunUserMessage(context.Background(), "inspect README.md and report", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) < 2 {
		t.Fatalf("requests=%d, want tool follow-up", len(*requests))
	}
	for i, req := range *requests {
		if req.MaxTokens != 2048 {
			t.Fatalf("request %d max_tokens=%d, want configured 2048", i, req.MaxTokens)
		}
	}
}

func TestAgentRetriesUnsupportedToolMarkupOnce(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return textSSE("<function=read_file>\n</function>\n</tool_call>")
		}
		if n == 2 {
			return toolSSE("read_file", `{"path":"README.md"}`)
		}
		return textSSE("verified")
	}, reg)
	defer closeServer()

	if err := ag.RunUserMessage(context.Background(), "inspect README.md and report", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 1 || len(*requests) < 3 || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("markup recovery failed: calls=%d requests=%d state=%+v", len(reader.calls), len(*requests), ag.RunState)
	}
}

func TestAgentFailureReplansAndRetries(t *testing.T) {
	tool := &scriptedTool{name: "read_file", outputs: []scriptedOutcome{{err: errors.New("temporary failure")}, {out: "fixed contents"}}}
	reg := tools.NewRegistry()
	reg.Register(tool)
	ag, _, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1, 2:
			return toolSSE("read_file", `{"path":"x.go"}`)
		case 3:
			return textSSE("checked")
		default:
			return textSSE("complete")
		}
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "fix x.go and verify the tests", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(ag.RunState.Failures) != 1 || ag.RunState.Retries != 1 || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("state=%+v", ag.RunState)
	}
}

func TestAgentTestFailureDiagnosesFixesAndVerifies(t *testing.T) {
	testTool := &scriptedTool{name: "run_tests", outputs: []scriptedOutcome{
		{out: "--- FAIL: TestRegression", err: errors.New("exit status 1")},
		{out: "ok ./..."},
	}}
	reg := tools.NewRegistry()
	reg.Register(testTool)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 || n == 2 {
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		}
		if n == 3 {
			return textSSE("tests pass after the fix")
		}
		return textSSE("verified")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "fix the regression and run the tests", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(ag.RunState.Failures) != 1 || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("test failure was not carried through verification: %+v", ag.RunState)
	}
}

func TestAgentFailedVerificationIsNotClearedByLaterRead(t *testing.T) {
	testTool := &scriptedTool{name: "run_tests", outputs: []scriptedOutcome{{out: "FAIL: TestRegression", err: errors.New("exit status 1")}}}
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(testTool)
	reg.Register(reader)
	ag, _, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		case 2:
			return toolSSE("read_file", `{"path":"score.go"}`)
		default:
			return textSSE("verified")
		}
	}, reg)
	defer closeServer()

	if err := ag.RunUserMessage(context.Background(), "fix Score and make the regression test pass", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Verification == VerificationPassed || ag.RunState.Phase == PhaseComplete {
		t.Fatalf("later read cleared failed verification: %+v", ag.RunState)
	}
	if len(ag.RunState.VerificationCriteria) != 1 || ag.RunState.VerificationCriteria[0].Status == VerificationPassed {
		t.Fatalf("verification criterion was overwritten: %+v", ag.RunState.VerificationCriteria)
	}
}

func TestAgentChecksAllAcceptanceCriteriaBeforeDone(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	search := &scriptedTool{name: "find_files"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(search)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("read_file", `{"path":"README.md"}`)
		case 2:
			return toolSSE("find_files", `{"name":"*_test.go"}`)
		case 3:
			return textSSE("both acceptance criteria checked")
		default:
			return textSSE("complete")
		}
	}, reg)
	defer closeServer()
	goal := "update the docs and ensure regression tests exist"
	if err := ag.RunUserMessage(context.Background(), goal, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(ag.RunState.ToolCalls) != 2 || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("premature completion: state=%+v", ag.RunState)
	}
	last := (*requests)[len(*requests)-2]
	found := false
	for _, msg := range last.Messages {
		if strings.Contains(msg.Content, "every applicable acceptance criterion") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("verification gate did not require all criteria")
	}
}

func TestAgentVerificationPassDoesNotCompleteWhenTestsCriterionIsMissing(t *testing.T) {
	writer := &scriptedTool{name: "str_replace"}
	tests := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(writer)
	reg.Register(tests)
	ag, _, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("str_replace", `{"path":"slug.go","old_string":"old","new_string":"new"}`)
		case 2:
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		default:
			return textSSE("verified")
		}
	}, reg)
	defer closeServer()

	goal := "Implement Slugify, add tests for each criterion, and run go test ./... before completion."
	if err := ag.RunUserMessage(context.Background(), goal, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Verification != VerificationPassed || ag.RunState.Phase == PhaseComplete || ag.RunState.canComplete() {
		t.Fatalf("verification pass incorrectly completed incomplete goal: %+v", ag.RunState)
	}
	if len(ag.RunState.AcceptanceCriteriaState) == 0 || ag.RunState.AcceptanceCriteriaState[1].Satisfied {
		t.Fatalf("missing tests criterion was marked satisfied: %+v", ag.RunState.AcceptanceCriteriaState)
	}
}

func TestAgentDoesNotPassExplicitlyFailedVerification(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&scriptedTool{name: "read_file"})
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"README.md"}`)
		}
		if n == 2 {
			return textSSE("implementation appears complete")
		}
		return textSSE("verification failed: the relevant test still fails")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "fix README.md and verify the relevant test", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Verification == VerificationPassed || ag.RunState.Phase == PhaseComplete {
		t.Fatalf("explicit verification failure was reported as success: %+v", ag.RunState)
	}
}

func TestAgentRequiresVerificationEvidenceAfterMutation(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&scriptedTool{name: "str_replace"})
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("str_replace", `{"path":"x.go","old_string":"old","new_string":"new"}`)
		}
		return textSSE("the change is complete")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "change x.go and verify it", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Verification == VerificationPassed || ag.RunState.Phase != PhaseBlocked || ag.RunState.Retries == 0 {
		t.Fatalf("mutation was verified without test evidence: requests=%d state=%+v", len(*requests), ag.RunState)
	}
}

func TestAgentRetryLimitStopsToolFailureRecovery(t *testing.T) {
	tool := &scriptedTool{name: "read_file", outputs: []scriptedOutcome{{err: errors.New("persistent failure")}}}
	reg := tools.NewRegistry()
	reg.Register(tool)
	ag, requests, closeServer := testAgent(t, func(int) string {
		return toolSSE("read_file", `{"path":"missing.go"}`)
	}, reg)
	defer closeServer()
	ag.MaxRetries = 1
	if err := ag.RunUserMessage(context.Background(), "recover from the read failure", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	tool.mu.Lock()
	toolCalls := len(tool.calls)
	tool.mu.Unlock()
	if toolCalls != 1 || ag.RunState.Retries != 1 || ag.RunState.Phase != PhaseBlocked || len(*requests) != 2 {
		t.Fatalf("retry limit failed: tool_calls=%d requests=%d state=%+v", toolCalls, len(*requests), ag.RunState)
	}
}

func TestAgentNoOpAlreadyCorrectStillVerifies(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	writer := &scriptedTool{name: "write_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(writer)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"correct.go"}`)
		}
		if n == 2 {
			return textSSE("the requested change is already present")
		}
		return textSSE("verified; no modification was necessary")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "fix correct.go if needed and verify it", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	writes := len(writer.calls)
	writer.mu.Unlock()
	if writes != 0 || ag.RunState.Verification != VerificationPassed {
		t.Fatalf("no-op was not verified cleanly: writes=%d state=%+v", writes, ag.RunState)
	}
}

func TestAgentReplansWhenAssumptionIsInvalid(t *testing.T) {
	first := &scriptedTool{name: "read_file", outputs: []scriptedOutcome{{out: "assumption invalid"}}}
	second := &scriptedTool{name: "find_files"}
	reg := tools.NewRegistry()
	reg.Register(first)
	reg.Register(second)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"expected.go"}`)
		}
		if n == 2 {
			return toolSSE("find_files", `{"name":"actual.go"}`)
		}
		if n == 3 {
			return textSSE("verified")
		}
		return textSSE("final")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "find the implementation and report", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(ag.RunState.ToolCalls) != 2 || ag.RunState.ToolCalls[1].Name != "find_files" {
		t.Fatalf("expected replanning, state=%+v", ag.RunState)
	}
}

func TestAgentIterationLimitStopsSafely(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&scriptedTool{name: "read_file"})
	ag, requests, closeServer := testAgent(t, func(int) string { return toolSSE("read_file", `{"path":"x"}`) }, reg)
	defer closeServer()
	ag.MaxIterations = 2
	if err := ag.RunUserMessage(context.Background(), "keep inspecting", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Phase != PhaseBlocked || len(*requests) != 2 {
		t.Fatalf("state=%+v requests=%d", ag.RunState, len(*requests))
	}
}

func TestAgentPermissionCannotBeBypassedByLoop(t *testing.T) {
	tool := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(tool)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("run_tests", `{"command":"go test ./..."}`)
		}
		if n == 2 {
			return textSSE("permission blocked")
		}
		return textSSE("final")
	}, reg)
	defer closeServer()
	ag.Permissions = permissions.New(permissions.ModeAsk, t.TempDir()+"/permissions.json")
	var denied bool
	if err := ag.RunUserMessage(context.Background(), "run the tests", func(e Event) {
		if e.Kind == EventPermission {
			denied = true
			e.Decision <- permissions.Deny
		}
	}); err != nil {
		t.Fatal(err)
	}
	tool.mu.Lock()
	calls := len(tool.calls)
	tool.mu.Unlock()
	if !denied || calls != 0 || ag.RunState.Failures[0].Category != tools.FailurePermissionDenied {
		t.Fatalf("permission bypass: denied=%v calls=%d state=%+v", denied, calls, ag.RunState)
	}
}
