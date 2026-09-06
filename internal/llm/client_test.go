package llm

import (
	"encoding/json"
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
