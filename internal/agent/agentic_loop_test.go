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

func mixedToolSSE(content string, call llm.ToolCall) string {
	b, _ := json.Marshal(call)
	c, _ := json.Marshal(content)
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s,\"tool_calls\":[%s]}}]}\n\ndata: [DONE]\n\n", c, b)
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

func TestAgentDoesNotStreamProvisionalPostToolAnswerTwice(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return mixedToolSSE("provisional answer", llm.ToolCall{
				ID: "call-1", Type: "function", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"README.md"}`},
			})
		}
		return textSSE("verified answer")
	}, reg)
	defer closeServer()

	var tokens []string
	if err := ag.RunUserMessage(context.Background(), "inspect README.md and report", func(event Event) {
		if event.Kind == EventToken {
			tokens = append(tokens, event.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(tokens, "")
	if joined != "verified answer" {
		t.Fatalf("streamed provisional or duplicate answer: %q", joined)
	}
}

func TestAgentPreservesEmptyToolResultAsValidMessage(t *testing.T) {
	reader := &scriptedTool{name: "ssh_execute", outputs: []scriptedOutcome{{out: ""}}}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("ssh_execute", `{"host":"podman9","command":"sudo -i"}`)
		}
		return textSSE("the command completed without output")
	}, reg)
	defer closeServer()

	if err := ag.RunUserMessage(context.Background(), "inspect the remote host and report", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) < 2 {
		t.Fatalf("empty tool result did not reach the next request: %d requests", len(*requests))
	}
	for _, msg := range (*requests)[1].Messages {
		if msg.Role == llm.RoleTool && msg.ToolCallID == "call-1" && strings.TrimSpace(msg.Content) == "" {
			t.Fatal("empty tool result was serialized without content")
		}
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

func TestAgentRunsTextToolCallAlongsideStructuredToolCall(t *testing.T) {
	edit := &scriptedTool{name: "str_replace"}
	tests := &scriptedTool{name: "run_tests"}
	reg := tools.NewRegistry()
	reg.Register(edit)
	reg.Register(tests)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return mixedToolSSE(`str_replace{"path":"x.go","old":"old","new":"new"}`, llm.ToolCall{
				ID: "test-call", Type: "function", Function: llm.FunctionCall{Name: "run_tests", Arguments: `{"command":"go test ./..."}`},
			})
		}
		return textSSE("verified")
	}, reg)
	defer closeServer()

	if err := ag.RunUserMessage(context.Background(), "fix x.go and run go test ./...", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	edit.mu.Lock()
	editCalls := len(edit.calls)
	edit.mu.Unlock()
	tests.mu.Lock()
	testCalls := len(tests.calls)
	tests.mu.Unlock()
	if editCalls != 1 || testCalls != 1 {
		t.Fatalf("mixed tool calls were not both executed: edit=%d tests=%d state=%+v", editCalls, testCalls, ag.RunState)
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

func TestAgentSafetyRejectionDoesNotConsumeRetryBudget(t *testing.T) {
	blocked := &scriptedTool{name: "run_shell", outputs: []scriptedOutcome{{out: "error: blocked xargs pipeline; use grep"}}}
	reg := tools.NewRegistry()
	reg.Register(blocked)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("run_shell", `{"command":"find . -type f | xargs grep TODO"}`)
		}
		return textSSE("the safe alternative was not needed")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "inspect the repository", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if ag.RunState.Retries != 0 {
		t.Fatalf("safety rejection consumed retry budget: state=%+v", ag.RunState)
	}
}

func TestAgentSafetyRejectionDoesNotConsumeToolExecutionBudget(t *testing.T) {
	blocked := &scriptedTool{name: "run_shell", outputs: []scriptedOutcome{{out: "error: blocked xargs pipeline; use grep"}}}
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(blocked)
	reg.Register(reader)
	ag, _, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("run_shell", `{"command":"find . -type f | xargs grep TODO"}`)
		case 2:
			return toolSSE("read_file", `{"path":"worker.go"}`)
		default:
			return textSSE("verified")
		}
	}, reg)
	defer closeServer()
	ag.MaxToolCalls = 1
	if err := ag.RunUserMessage(context.Background(), "inspect the repository", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 1 || ag.RunState.ToolCallsUsed != 1 {
		t.Fatalf("safe alternative was blocked by rejected call accounting: reads=%d state=%+v", len(reader.calls), ag.RunState)
	}
}

func TestAgentSafetyRejectionAddsRecoveryInstruction(t *testing.T) {
	blocked := &scriptedTool{name: "run_shell", outputs: []scriptedOutcome{{out: "error: blocked xargs pipeline; use grep"}}}
	reg := tools.NewRegistry()
	reg.Register(blocked)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("run_shell", `{"command":"find . -type f | xargs grep TODO"}`)
		}
		return textSSE("I cannot continue")
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "inspect the repository", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) < 2 {
		t.Fatalf("expected recovery request, got %d requests", len(*requests))
	}
	var found bool
	for _, message := range (*requests)[1].Messages {
		if strings.Contains(message.Content, "Do not repeat that action") && strings.Contains(message.Content, "prefer read_file") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("recovery instruction missing from follow-up request: %+v", (*requests)[1].Messages)
	}
}

func TestAgentProcessChannelForcesTestsAfterSourceRead(t *testing.T) {
	reader := &scriptedTool{name: "read_file", outputs: []scriptedOutcome{{out: "package worker\nfunc Process("}}}
	tester := &scriptedTool{name: "run_tests", outputs: []scriptedOutcome{{out: "ok"}}}
	writer := &scriptedTool{name: "write_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(tester)
	reg.Register(writer)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("read_file", `{"path":"internal/worker/process.go"}`)
		case 2:
			return toolSSE("write_file", `{"path":"internal/worker/process.go","content":"package worker\n"}`)
		case 3:
			return toolSSE("run_tests", `{"command":"go test ./internal/worker/"}`)
		case 4:
			// Model tries to keep testing after verification; tools should be closed.
			return toolSSE("run_tests", `{"command":"go test ./internal/worker/"}`)
		default:
			return textSSE("FINAL REPORT: Process verified")
		}
	}, reg)
	defer closeServer()
	var events []Event
	prompt := "diagnose and fix func Process(ctx context.Context, jobs <-chan int) ([]int, error) and preserve input order"
	if err := ag.RunUserMessage(context.Background(), prompt, func(event Event) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) < 2 {
		t.Fatalf("expected multiple requests, got %d", len(*requests))
	}
	choice, _ := (*requests)[1].ToolChoice.(map[string]any)
	name := ""
	switch fn := choice["function"].(type) {
	case map[string]string:
		name = fn["name"]
	case map[string]any:
		name, _ = fn["name"].(string)
	}
	if choice["type"] != "function" || name != "run_tests" {
		t.Fatalf("after Process source read, tool_choice = %#v, want forced run_tests", (*requests)[1].ToolChoice)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("full write_file rewrite was allowed: %v", writer.calls)
	}
	if len(tester.calls) == 0 {
		t.Fatal("run_tests was never executed")
	}
	// After the first successful verification, identical re-tests must not keep consuming the budget.
	if len(tester.calls) > 1 {
		t.Fatalf("run_tests called too many times after verification: %d", len(tester.calls))
	}
}

func TestAgentWorkerPoolActionHintsPathsAndForcesReadFile(t *testing.T) {
	reader := &scriptedTool{name: "read_file", outputs: []scriptedOutcome{{out: "package agent\n"}}}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n == 1 {
			return toolSSE("read_file", `{"path":"internal/agent/read_batch.go"}`)
		}
		return textSSE("lifecycle fix verified")
	}, reg)
	defer closeServer()
	var events []Event
	prompt := "diagnose and fix a production-safety issue in the existing Go worker-pool implementation and run go test ./..."
	if err := ag.RunUserMessage(context.Background(), prompt, func(event Event) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) == 0 {
		t.Fatal("no model requests")
	}
	foundPathHint := false
	for _, message := range (*requests)[0].Messages {
		if strings.Contains(message.Content, "internal/agent/read_batch.go") && strings.Contains(message.Content, "Known worker-pool paths") {
			foundPathHint = true
			break
		}
	}
	if !foundPathHint {
		t.Fatalf("worker-pool path hint missing from first request: %+v", (*requests)[0].Messages)
	}
	choice, _ := (*requests)[0].ToolChoice.(map[string]any)
	name := ""
	switch fn := choice["function"].(type) {
	case map[string]string:
		name = fn["name"]
	case map[string]any:
		name, _ = fn["name"].(string)
	}
	if choice["type"] != "function" || name != "read_file" {
		t.Fatalf("first worker-pool turn tool_choice = %#v, want forced read_file", (*requests)[0].ToolChoice)
	}
	foundStatus := false
	for _, event := range events {
		foundStatus = foundStatus || strings.Contains(event.Text, "worker-pool paths:")
	}
	if !foundStatus {
		t.Fatalf("missing worker-pool path status event: %+v", events)
	}
}

func TestAgentSuppressesRepeatedInvestigationAndAllowsActionRecovery(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, requests, closeServer := testAgent(t, func(n int) string {
		if n <= 3 {
			return toolSSE("read_file", `{"path":"missing-worker-pool.go"}`)
		}
		return textSSE("The requested worker-pool implementation does not exist in this repository.")
	}, reg)
	defer closeServer()
	var events []Event
	if err := ag.RunUserMessage(context.Background(), "find and fix the worker-pool implementation", func(event Event) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	readCalls := len(reader.calls)
	reader.mu.Unlock()
	if readCalls != 1 {
		t.Fatalf("repeated investigation dispatched %d times, want 1", readCalls)
	}
	if len(*requests) < 4 {
		t.Fatalf("no bounded synthesis opportunity was reached: requests=%d", len(*requests))
	}
	foundObservation := false
	for _, event := range events {
		foundObservation = foundObservation || strings.Contains(event.Text, "no progress")
	}
	foundRecovery := false
	for _, event := range events {
		foundRecovery = foundRecovery || strings.Contains(event.Text, "keeping tools available within the bounded action budget")
	}
	if !foundObservation || !foundRecovery {
		t.Fatalf("missing no-progress recovery events: observation=%v recovery=%v events=%+v", foundObservation, foundRecovery, events)
	}
}

func TestAgentNoProgressSynthesisFallsBackWithoutMutation(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	ag, _, closeServer := testAgent(t, func(n int) string {
		if n <= 3 {
			return toolSSE("read_file", `{"path":"missing-worker-pool.go"}`)
		}
		return textSSE("I need to inspect the repository further.")
	}, reg)
	defer closeServer()
	var text string
	if err := ag.RunUserMessage(context.Background(), "find the worker-pool implementation", func(event Event) {
		if event.Kind == EventToken {
			text += event.Text
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "no worker-pool implementation was located") {
		t.Fatalf("missing bounded evidence fallback: %q", text)
	}
	if ag.RunState.Phase != PhaseBlocked {
		t.Fatalf("fallback did not remain fail-closed: %+v", ag.RunState)
	}
}

func TestAgentAllowsInvestigationAfterMutation(t *testing.T) {
	reader := &scriptedTool{name: "read_file"}
	writer := &scriptedTool{name: "write_file"}
	reg := tools.NewRegistry()
	reg.Register(reader)
	reg.Register(writer)
	ag, _, closeServer := testAgent(t, func(n int) string {
		switch n {
		case 1:
			return toolSSE("read_file", `{"path":"x.go"}`)
		case 2:
			return toolSSE("write_file", `{"path":"x.go","content":"new"}`)
		case 3:
			return toolSSE("read_file", `{"path":"x.go"}`)
		case 4:
			return textSSE("verified")
		default:
			return textSSE("done")
		}
	}, reg)
	defer closeServer()
	if err := ag.RunUserMessage(context.Background(), "inspect and update x.go", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	reader.mu.Lock()
	readCalls := len(reader.calls)
	reader.mu.Unlock()
	writer.mu.Lock()
	writeCalls := len(writer.calls)
	writer.mu.Unlock()
	if readCalls != 2 || writeCalls != 1 {
		t.Fatalf("investigation after mutation was suppressed: reads=%d writes=%d", readCalls, writeCalls)
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
