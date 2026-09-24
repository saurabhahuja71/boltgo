package agent

import "testing"

func TestExtractToolCallsFromContent(t *testing.T) {
	known := map[string]struct{}{
		"read_file":  {},
		"list_dir":   {},
		"find_files": {},
	}
	raw := `{"name": "read_file", "arguments": {"path": "repo/dbope/sidb/oracle-database-operator/README.md"}}`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d rest=%q", len(calls), rest)
	}
	if calls[0].Function.Name != "read_file" {
		t.Fatalf("name %q", calls[0].Function.Name)
	}
	if rest != "" {
		t.Fatalf("want empty rest, got %q", rest)
	}
	if !contains(calls[0].Function.Arguments, "README.md") {
		t.Fatalf("args %q", calls[0].Function.Arguments)
	}

	// non-tool chat
	calls, rest = extractToolCallsFromContent("hello there", known)
	if len(calls) != 0 || rest != "hello there" {
		t.Fatalf("unexpected chat parse: calls=%d rest=%q", len(calls), rest)
	}
}

func TestExtractLlamaMarkupToolCalls(t *testing.T) {
	known := map[string]struct{}{"list_dir": {}, "grep": {}}
	raw := `<tool_call><function=list_dir><parameter=path>/home/sauahuja/covered_call_bot</parameter></tool_call>` +
		`<tool_call><function=grep><parameter=pattern>2026-08-28</parameter><parameter=path>/home/sauahuja/covered_call_bot</parameter></tool_call>`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 2 || rest != "" {
		t.Fatalf("calls=%d rest=%q", len(calls), rest)
	}
	if calls[0].Function.Name != "list_dir" || !contains(calls[0].Function.Arguments, "covered_call_bot") {
		t.Fatalf("first call=%+v", calls[0])
	}
	if calls[1].Function.Name != "grep" || !contains(calls[1].Function.Arguments, "2026-08-28") {
		t.Fatalf("second call=%+v", calls[1])
	}
}

func TestExtractLlamaMarkupWithoutFunctionClose(t *testing.T) {
	known := map[string]struct{}{"list_dir": {}}
	raw := `<tool_call><function=list_dir><parameter=path>/tmp</tool_call>`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 1 || rest != "" || !contains(calls[0].Function.Arguments, `"path":"/tmp"`) {
		t.Fatalf("calls=%d rest=%q args=%q", len(calls), rest, calls[0].Function.Arguments)
	}
}

func TestExtractQwenMarkupWithOpenParametersAndRepeatedCalls(t *testing.T) {
	known := map[string]struct{}{"read_file": {}, "list_dir": {}}
	raw := `No, I haven't found any reference yet.
<tool_call> <function=read_file> <parameter=path> covered/README.md </tool_call> <tool_call>
<function=read_file> <parameter=path> .github/workflows/covered_call.yml </tool_call> <tool_call>
<function=list_dir> <parameter=path> covered/scripts </tool_call>`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 3 {
		t.Fatalf("calls=%d rest=%q", len(calls), rest)
	}
	if !contains(calls[0].Function.Arguments, `"path":"covered/README.md"`) ||
		!contains(calls[1].Function.Arguments, `"path":".github/workflows/covered_call.yml"`) ||
		!contains(calls[2].Function.Arguments, `"path":"covered/scripts"`) {
		t.Fatalf("unexpected args: %q, %q, %q", calls[0].Function.Arguments, calls[1].Function.Arguments, calls[2].Function.Arguments)
	}
	if rest != "No, I haven't found any reference yet." {
		t.Fatalf("rest=%q", rest)
	}
}

func TestUnsupportedToolMarkupIsDetectedWithoutChangingProtocol(t *testing.T) {
	if !hasUnsupportedToolMarkup("<function=repo_map>\n</function>\n</tool_call>") {
		t.Fatal("expected unsupported template markup to be detected")
	}
	if hasUnsupportedToolMarkup("ordinary answer mentioning tool_calls") {
		t.Fatal("ordinary prose was misclassified as tool markup")
	}
}

func TestStreamBridgeSuppressesTextToolMarkup(t *testing.T) {
	var got string
	bridge := &streamBridge{emit: func(ev Event) {
		if ev.Kind == EventToken {
			got += ev.Text
		}
	}}
	bridge.OnToken("Before ")
	bridge.OnToken("<tool_call><function=read_file>")
	bridge.OnToken("<parameter=path>main.go</tool_call>")
	bridge.OnToken(" after")
	bridge.Flush()
	if got != "Before  after" {
		t.Fatalf("visible stream=%q", got)
	}
}

func TestNormalizeToolNameRepairsConcatenatedProviderNames(t *testing.T) {
	known := map[string]struct{}{"list_dir": {}, "read_file": {}}
	for input, want := range map[string]string{
		"list_dir":           "list_dir",
		"list_dirread_file":  "list_dir",
		"read_fileread_file": "read_file",
		"unknown":            "unknown",
	} {
		if got := normalizeToolName(input, known); got != want {
			t.Fatalf("normalizeToolName(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestSanitizeReadFileArgumentsNormalizesProviderWrappers(t *testing.T) {
	for _, test := range []struct {
		name string
		args string
		want string
	}{
		{name: "file path alias", args: `{"file_path":"main.py"}`, want: `{"path":"main.py"}`},
		{name: "file alias", args: `{"file":"main.py"}`, want: `{"path":"main.py"}`},
		{name: "nested object", args: `{"parameters":{"path":"main.py"}}`, want: `{"path":"main.py"}`},
		{name: "nested string", args: `{"arguments":"{\"path\":\"main.py\"}"}`, want: `{"path":"main.py"}`},
		{name: "bare path", args: `main.py`, want: `{"path":"main.py"}`},
		{name: "quoted bare path", args: `"README.md"`, want: `{"path":"README.md"}`},
		{name: "missing remains missing", args: `{}`, want: `{}`},
	} {
		if got := sanitizeToolArgsJSON("read_file", test.args); got != test.want {
			t.Fatalf("%s: got %s want %s", test.name, got, test.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
