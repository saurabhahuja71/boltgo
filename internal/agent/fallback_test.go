package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

type failingFetch struct{}

func (failingFetch) Name() string                                { return "fetch" }
func (failingFetch) Description() string                         { return "test fetch" }
func (failingFetch) Schema() map[string]any                      { return map[string]any{"type": "object"} }
func (failingFetch) Run(context.Context, string) (string, error) { return "HTTP 404 Not Found", nil }
func (failingFetch) RunDetailed(context.Context, string) tools.ExecutionResult {
	return tools.ExecutionResult{Output: "HTTP 404 Not Found", Category: tools.FailureNotFound, HTTPStatus: 404, ResourceNotFound: true}
}

func TestGitHubActions404FallbackPreservesIntentAndContinuesLoop(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			call := llm.ToolCall{ID: "fetch-1", Type: "function", Function: llm.FunctionCall{Name: "fetch", Arguments: `{"url":"https://github.com/acme/widget/actions/runs/123/job/456"}`}}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []llm.ToolCall{call}}}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"retrieved logs\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	reg := tools.NewRegistry()
	reg.Register(failingFetch{})
	cfg := config.Default()
	cfg.Provider, cfg.BaseURL, cfg.Model, cfg.Workspace, cfg.PermissionMode = "custom", server.URL, "test", t.TempDir(), "allow"
	ag := New(cfg, llm.New(server.URL, ""), reg)
	ag.GitHubFallback = tools.GitHubFallbackDeps{
		Capabilities: func(context.Context) tools.CommandCapabilities {
			return tools.CommandCapabilities{GHInstalled: true, GHAuthenticated: true}
		},
		RunCommand: func(context.Context, []string) tools.ExecutionResult {
			return tools.ExecutionResult{Output: "job log output", Category: tools.FailureSuccess}
		},
	}
	var events []Event
	if err := ag.RunUserMessage(context.Background(), "download this GitHub Actions job log", func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("model requests=%d, want fallback follow-up plus verification", requests)
	}
	if !strings.Contains(ag.History[1].Content, "download this GitHub Actions job log") {
		t.Fatalf("intent missing: %+v", ag.History[1])
	}
	toolFound := false
	for _, message := range ag.History {
		if message.Role == llm.RoleTool && strings.Contains(message.Content, "job log output") && strings.Contains(message.Content, "resource_not_found") {
			toolFound = true
		}
	}
	if !toolFound {
		t.Fatalf("structured fallback result missing: %+v", ag.History)
	}
	statusFound := false
	for _, event := range events {
		if event.Kind == EventStatus && strings.Contains(event.Text, "trying GitHub Actions fallback") {
			statusFound = true
		}
	}
	if !statusFound {
		t.Fatalf("fallback status event missing: %+v", events)
	}
}
