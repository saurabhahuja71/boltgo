package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestFreshRequestDoesNotInheritSavedConversation(t *testing.T) {
	const sentinel = "THIS-MUST-NOT-APPEAR-IN-FRESH-CONTEXT"
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Provider = "custom"
	cfg.BaseURL = server.URL
	cfg.Workspace = workspace
	cfg.EnableTools = false
	cfg.SystemPrompt = "test system"

	fresh := New(cfg, llm.New(server.URL, ""), tools.NewRegistry())
	if err := fresh.RunUserMessage(context.Background(), "fresh request", func(Event) {}); err != nil {
		t.Fatal(err)
	}

	old := New(cfg, llm.New(server.URL, ""), tools.NewRegistry())
	old.History = append(old.History, llm.Message{Role: llm.RoleUser, Content: sentinel})
	path := filepath.Join(workspace, ".bolt", "sessions", "latest.json")
	if _, err := old.SaveSessionPath(path); err != nil {
		t.Fatal(err)
	}
	resumed := New(cfg, llm.New(server.URL, ""), tools.NewRegistry())
	if err := resumed.LoadSessionPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resumed.RunUserMessage(context.Background(), "resumed request", func(Event) {}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	if strings.Contains(requests[0], sentinel) {
		t.Fatal("fresh request inherited saved conversation")
	}
	if !strings.Contains(requests[1], sentinel) {
		t.Fatal("resumed request omitted saved conversation")
	}
}
