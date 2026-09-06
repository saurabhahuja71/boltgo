package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFunctionCallArgumentsStringOrObject(t *testing.T) {
	var asStr FunctionCall
	if err := json.Unmarshal([]byte(`{"name":"str_replace","arguments":"{\"path\":\"a\"}"}`), &asStr); err != nil {
		t.Fatal(err)
	}
	if asStr.Name != "str_replace" || !strings.Contains(asStr.Arguments, "path") {
		t.Fatalf("string form: %+v", asStr)
	}

	var asObj FunctionCall
	if err := json.Unmarshal([]byte(`{"name":"str_replace","arguments":{"path":"README.md","old_string":"x","new_string":"y"}}`), &asObj); err != nil {
		t.Fatal(err)
	}
	if asObj.Name != "str_replace" {
		t.Fatalf("name %q", asObj.Name)
	}
	if !strings.Contains(asObj.Arguments, "README.md") || !strings.Contains(asObj.Arguments, "old_string") {
		t.Fatalf("object form args %q", asObj.Arguments)
	}
}

func TestFunctionCallArgumentsAcceptProviderAliases(t *testing.T) {
	for _, raw := range []string{
		`{"name":"read_file","parameters":{"path":"main.py"}}`,
		`{"name":"read_file","args":{"path":"main.py"}}`,
		`{"name":"read_file","path":"main.py"}`,
	} {
		var call FunctionCall
		if err := json.Unmarshal([]byte(raw), &call); err != nil {
			t.Fatal(err)
		}
		if call.Arguments != `{"path":"main.py"}` {
			t.Fatalf("%s decoded arguments=%q", raw, call.Arguments)
		}
	}
}

func TestToolCallArgumentsAcceptTopLevelProviderShape(t *testing.T) {
	var call ToolCall
	if err := json.Unmarshal([]byte(`{"name":"read_file","parameters":{"path":"main.py"}}`), &call); err != nil {
		t.Fatal(err)
	}
	if call.Function.Name != "read_file" || call.Function.Arguments != `{"path":"main.py"}` {
		t.Fatalf("decoded top-level tool call: %+v", call)
	}
}

func TestStreamChunkAcceptsFullMessage(t *testing.T) {
	// Shape the old sglang-toolcall-proxy emitted for stream:true clients.
	raw := `{
	  "choices": [{
	    "index": 0,
	    "message": {
	      "role": "assistant",
	      "content": "",
	      "tool_calls": [{
	        "id": "call_1",
	        "type": "function",
	        "function": {
	          "name": "str_replace",
	          "arguments": "{\"path\":\"README.md\",\"old_string\":\"OLD\",\"new_string\":\"NEW\"}"
	        }
	      }]
	    },
	    "finish_reason": "tool_calls"
	  }]
	}`
	var chunk streamChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatal(err)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Message == nil {
		t.Fatalf("want message on choice, got %+v", chunk)
	}
	tc := chunk.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "str_replace" {
		t.Fatalf("tool_calls %+v", tc)
	}
	if !strings.Contains(tc[0].Function.Arguments, "OLD") {
		t.Fatalf("args %q", tc[0].Function.Arguments)
	}
}

func TestStreamChunkReadsUsage(t *testing.T) {
	var chunk streamChunk
	if err := json.Unmarshal([]byte(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`), &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Usage == nil || chunk.Usage.TotalTokens != 19 {
		t.Fatalf("usage = %#v", chunk.Usage)
	}
}

func TestChatStreamAccumulatesSplitToolArgumentsBeforeReturning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"read_file\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"pa\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"th\\\":\\\"main.py\\\"}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}
	message, err := client.ChatStream(context.Background(), ChatRequest{Model: "test"}, testStreamHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "read_file" ||
		message.ToolCalls[0].Function.Arguments != `{"path":"main.py"}` {
		t.Fatalf("assembled tool call: %+v", message.ToolCalls)
	}
}

type testStreamHandler struct{}

func (testStreamHandler) OnToken(string)                {}
func (testStreamHandler) OnToolCallDelta(int, ToolCall) {}
func (testStreamHandler) OnStatus(string)               {}

func TestMergeToolArgumentDoesNotEraseAccumulatedFragments(t *testing.T) {
	if got := mergeToolArgument(`{"path":"main`, `{"path":"main.py"}`); got != `{"path":"main.py"}` {
		t.Fatalf("cumulative merge=%q", got)
	}
	if got := mergeToolArgument(`{"path":"main`, `.py"}`); got != `{"path":"main.py"}` {
		t.Fatalf("fragment merge=%q", got)
	}
}

func TestChatStreamMergesCumulativeDeltaArgumentsWithoutDuplication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Some providers re-send the full arguments string on every delta frame.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"ma\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"main.py\\\"}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}
	message, err := client.ChatStream(context.Background(), ChatRequest{Model: "test"}, testStreamHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool call: %+v", message.ToolCalls)
	}
	if message.ToolCalls[0].Function.Arguments != `{"path":"main.py"}` {
		t.Fatalf("cumulative delta args duplicated or lost: %q", message.ToolCalls[0].Function.Arguments)
	}
}

func TestChatStreamKeepsMultipleToolCallsIndependentByID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"id":"A","index":0,"function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"B","index":1,"function":{"name":"read_file","arguments":"{\"path\":\"database.py\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"A","index":0,"function":{"arguments":"th\":\"main.py\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"C","index":2,"function":{"name":"read_file","arguments":"{\"path\":\"requirements.txt\"}"}}]}}]}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}
	message, err := client.ChatStream(context.Background(), ChatRequest{Model: "test"}, testStreamHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 3 {
		t.Fatalf("want three calls, got %+v", message.ToolCalls)
	}
	want := []string{`{"path":"main.py"}`, `{"path":"database.py"}`, `{"path":"requirements.txt"}`}
	for i, call := range message.ToolCalls {
		if call.Function.Name != "read_file" || call.Function.Arguments != want[i] {
			t.Fatalf("call %d = %+v, want read_file(%s)", i, call, want[i])
		}
	}
}

func TestChatStreamUsesIndexesWhenIDsAreOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{\"pa"}},{"index":1,"function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}},{"index":1,"function":{"arguments":"th\":\"b\"}"}}]}}]}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}
	message, err := client.ChatStream(context.Background(), ChatRequest{Model: "test"}, testStreamHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 2 || message.ToolCalls[0].Function.Arguments != `{"path":"a"}` || message.ToolCalls[1].Function.Arguments != `{"path":"b"}` {
		t.Fatalf("indexed calls were merged: %+v", message.ToolCalls)
	}
}
