package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func dailyTestAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	a := New(cfg, llm.New("http://127.0.0.1:1", "test"), tools.DefaultBuiltins(false))
	a.EnableDailyMode()
	return a
}

func TestDailyModeUsesCompatibilityWithoutGoalGraph(t *testing.T) {
	a := dailyTestAgent(t)
	if !a.DailyMode || !a.CompatibilityMode || a.Scheduler != nil || a.ModeName() != "Daily" {
		t.Fatalf("daily mode daily=%v compatibility=%v scheduler=%v mode=%q", a.DailyMode, a.CompatibilityMode, a.Scheduler != nil, a.ModeName())
	}
	a.DisableDailyMode()
	if a.DailyMode || a.ModeName() != "Compatibility" {
		t.Fatalf("disable daily changed compatibility unexpectedly: daily=%v mode=%q", a.DailyMode, a.ModeName())
	}
}

func TestDailyCheckpointAndHandoffUseAuthoritativeState(t *testing.T) {
	a := dailyTestAgent(t)
	a.RunState.reset("implement the change and run go test ./...")
	a.RunState.addObservation("write_file", `{"path":"calc/calc.go"}`, tools.ExecutionResult{Category: tools.FailureSuccess, Output: "wrote calc/calc.go"})
	a.RunState.addObservation("run_tests", `{"command":"go test ./..."}`, tools.ExecutionResult{Category: tools.FailureCommand, Output: "FAIL: TestAverage"})
	checkpoint := a.DailyCheckpoint()
	handoff := a.DailyHandoff("bounded autonomy limit reached")
	for _, text := range []string{checkpoint, handoff} {
		for _, want := range []string{"calc/calc.go", "go test ./...", "command_failed", "tests/checks", "next:"} {
			if !strings.Contains(text, want) {
				t.Fatalf("daily summary missing %q:\n%s", want, text)
			}
		}
	}
	if strings.Contains(checkpoint, "model says") || strings.Contains(handoff, "chain of thought") {
		t.Fatalf("summary included model prose: %q", checkpoint)
	}
}

func TestDailyVerificationNeverRunsWhenWorkRemains(t *testing.T) {
	a := dailyTestAgent(t)
	a.RunState.reset("implement a change and run go test ./...")
	var events []Event
	if err := a.RunDailyVerification(context.Background(), func(ev Event) { events = append(events, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(a.RunState.ToolCalls) != 0 {
		t.Fatalf("verification ran with unfinished work: %+v", a.RunState.ToolCalls)
	}
	if len(events) != 1 || !strings.Contains(events[0].Text, "not currently eligible") {
		t.Fatalf("unexpected safe verification handoff: %+v", events)
	}
}

func TestDailyUnavailableBeforeFirstTurnIsSavedAndClassified(t *testing.T) {
	a := dailyTestAgent(t)
	var saves atomic.Int32
	a.SetDailyPersistence(func() error { saves.Add(1); return nil })
	err := a.RunUserMessage(context.Background(), "explain the project structure", func(Event) {})
	if err == nil || llm.ProviderErrorClassOf(err) != llm.ProviderConnectionRefused {
		t.Fatalf("error=%v class=%s", err, llm.ProviderErrorClassOf(err))
	}
	if a.ProviderState != llm.ProviderUnavailable || a.RunState.Phase != PhaseBlocked || saves.Load() == 0 {
		t.Fatalf("state=%s phase=%s saves=%d", a.ProviderState, a.RunState.Phase, saves.Load())
	}
}

func TestDailyResetBeforeDispatchRetriesWithoutDuplicateOutput(t *testing.T) {
	var chats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"test"}]}`)
		case "/chat/completions":
			if chats.Add(1) == 1 {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.BaseURL, cfg.Model, cfg.Workspace = srv.URL, "test", t.TempDir()
	a := New(cfg, llm.New(srv.URL, ""), tools.DefaultBuiltins(false))
	a.EnableDailyMode()
	var tokens string
	if err := a.RunUserMessage(context.Background(), "explain the project structure", func(ev Event) {
		if ev.Kind == EventToken {
			tokens += ev.Text
		}
	}); err != nil {
		t.Fatal(err)
	}
	if chats.Load() != 2 || tokens != "ok" || a.ProviderState != llm.ProviderConnected {
		t.Fatalf("chats=%d tokens=%q state=%s", chats.Load(), tokens, a.ProviderState)
	}
}

func TestDailyModelUnavailableDoesNotStartChat(t *testing.T) {
	var chats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"other"}]}`)
			return
		}
		chats.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.BaseURL, cfg.Model, cfg.Workspace = srv.URL, "test", t.TempDir()
	a := New(cfg, llm.New(srv.URL, ""), tools.DefaultBuiltins(false))
	a.EnableDailyMode()
	err := a.RunUserMessage(context.Background(), "explain the project structure", func(Event) {})
	if llm.ProviderErrorClassOf(err) != llm.ProviderModelUnavailable || chats.Load() != 0 {
		t.Fatalf("err=%v class=%s chats=%d", err, llm.ProviderErrorClassOf(err), chats.Load())
	}
}
