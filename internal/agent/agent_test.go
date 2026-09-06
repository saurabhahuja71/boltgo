package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestIsActionRequest(t *testing.T) {
	yes := []string{"can you do it", "do it", "apply the changes", "please implement it", "create a branch and commit"}
	no := []string{"hi", "can you read the readme yes or no", "what is SEO", "explain SEO friendly docs"}
	for _, s := range yes {
		if !isActionRequest(s) {
			t.Errorf("want action: %q", s)
		}
	}
	for _, s := range no {
		if isActionRequest(s) {
			t.Errorf("want non-action: %q", s)
		}
	}
}

func TestIsTrivialChat(t *testing.T) {
	yes := []string{
		"hi", "Hi!", "hello", "hey there", "thanks", "good morning",
		"how are you?", "what's up", "yo",
	}
	no := []string{
		"list the files here",
		"read README.md",
		"hi, please list dir",
		"fix the bug in main.go",
		"what files are in this folder",
		"",
	}
	for _, s := range yes {
		if !isTrivialChat(s) {
			t.Errorf("expected trivial: %q", s)
		}
	}
	for _, s := range no {
		if isTrivialChat(s) {
			t.Errorf("expected non-trivial: %q", s)
		}
	}
}

func TestIsLinkCheckRequest(t *testing.T) {
	if !isLinkCheckRequest("check links in this repo and it shud be workin ones") {
		t.Fatal("expected link check")
	}
	if !isLinkCheckRequest("check links in this documentation it should work") {
		t.Fatal("expected documentation link check")
	}
	if isLinkCheckRequest("how does linking work in go") {
		t.Fatal("not a link check")
	}
}

func TestExtractHTTPURLs(t *testing.T) {
	text := `see https://example.com/foo and http://golang.org/pkg. also (https://x.test/a).`
	got := extractHTTPURLs(text)
	if len(got) < 2 {
		t.Fatalf("urls: %v", got)
	}
}

func TestIsLocalOrPrivateURL(t *testing.T) {
	locals := []string{
		"http://localhost:8080/ords",
		"http://127.0.0.1:8443/health",
		"https://[::1]:8484/x",
		"http://192.168.1.5/app",
	}
	for _, u := range locals {
		if !isLocalOrPrivateURL(u) {
			t.Errorf("want local: %s", u)
		}
	}
	if isLocalOrPrivateURL("https://github.com/oracle/example") {
		t.Fatal("github should be external")
	}
}

func TestIsTemplatePlaceholderURL(t *testing.T) {
	if !isTemplatePlaceholderURL("https://$PROMETHEUS_SVC/api/v1/query") {
		t.Fatal("want template")
	}
	if isTemplatePlaceholderURL("https://prometheus.io/docs") {
		t.Fatal("real host")
	}
}

func TestIsShellOnlyAssistantText(t *testing.T) {
	bare := `grep https?:\/\/ . --files-with-matches | xargs -I {} grep -oP 'https?://' {} | sort -u`
	if !isShellOnlyAssistantText(bare) {
		t.Fatal("expected shell-only")
	}
	if isShellOnlyAssistantText("I checked three URLs and two are fine.") {
		t.Fatal("prose should not be shell-only")
	}
}

func TestRecoverShellishToGrep(t *testing.T) {
	known := map[string]struct{}{"grep": {}, "run_shell": {}, "fetch": {}}
	bare := `grep https?:\/\/ . --files-with-matches | xargs -I {} grep -oP 'https?://' {} | sort -u`
	calls, rest, note := recoverShellishContent(bare, known)
	if len(calls) != 1 || calls[0].Function.Name != "grep" {
		t.Fatalf("want grep recovery, got %+v note=%q", calls, note)
	}
	if rest != "" {
		t.Fatalf("rest should be empty, got %q", rest)
	}
}

type recordingTool struct {
	mu   sync.Mutex
	args []string
}

func (r *recordingTool) Name() string        { return "read_file" }
func (r *recordingTool) Description() string { return "test reader" }
func (r *recordingTool) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"path"}}
}
func (r *recordingTool) Run(_ context.Context, args string) (string, error) {
	r.mu.Lock()
	r.args = append(r.args, args)
	r.mu.Unlock()
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	if in.Path == "database.py" {
		return "", fmt.Errorf("synthetic failure")
	}
	return "contents of " + in.Path, nil
}

func TestReviewCurrentProjectDispatchesIndependentToolCalls(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			frame := strings.ReplaceAll(`data: {"choices":[{"delta":{"tool_calls":[{"id":"A","index":0,"function":{"name":"read_file","arguments":"{\"path\":\"main.py\"}"}},{"id":"B","index":1,"function":{"name":"read_file","arguments":"{\"path\":\"database.py\"}"}},{"id":"C","index":2,"function":{"name":"read_file","arguments":"{\"path\":\"requirements.txt\"}"}}]}}]}\n\ndata: [DONE]\n\n`, `\n`, "\n")
			_, _ = w.Write([]byte(frame))
			return
		}
		frame := strings.ReplaceAll(`data: {"choices":[{"delta":{"content":"done"}}]}\n\ndata: [DONE]\n\n`, `\n`, "\n")
		_, _ = w.Write([]byte(frame))
	}))
	defer server.Close()

	workspace, err := os.MkdirTemp("", "agenterm-dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workspace)
	recorder := &recordingTool{}
	registry := tools.NewRegistry()
	registry.Register(recorder)
	cfg := config.Default()
	cfg.BaseURL = server.URL
	cfg.Model = "test"
	cfg.Workspace = workspace
	cfg.PermissionMode = "allow"
	agent := New(cfg, nil, registry)
	// New receives a client separately in production; use the test server client.
	agent.Client = llm.New(server.URL, "")
	var events []Event
	if err := agent.RunUserMessage(context.Background(), "review current project", func(e Event) {
		events = append(events, e)
	}); err != nil {
		t.Fatal(err)
	}

	recorder.mu.Lock()
	got := append([]string(nil), recorder.args...)
	recorder.mu.Unlock()
	want := []string{`{"path":"main.py"}`, `{"path":"database.py"}`, `{"path":"requirements.txt"}`}
	if len(got) != len(want) {
		t.Fatalf("dispatch count=%d args=%v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch %d args=%q want %q", i, got[i], want[i])
		}
	}
	toolEnds := 0
	for _, e := range events {
		if e.Kind == EventToolEnd {
			toolEnds++
		}
	}
	if toolEnds != 3 {
		t.Fatalf("want one independent completion per call, got %d", toolEnds)
	}
}
