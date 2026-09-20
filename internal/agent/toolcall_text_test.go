package agent

import (
	"strings"
	"testing"
)

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

func TestExtractNamedToolCallFromProviderText(t *testing.T) {
	known := map[string]struct{}{"str_replace": {}}
	raw := `The implementation is already correct. str_replace{"path":"calc/calc_test.go","old":"old","new":"new"}`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 1 || calls[0].Function.Name != "str_replace" {
		t.Fatalf("calls=%+v rest=%q", calls, rest)
	}
	if !strings.Contains(calls[0].Function.Arguments, "calc/calc_test.go") {
		t.Fatalf("arguments=%q", calls[0].Function.Arguments)
	}
	if rest != "The implementation is already correct." {
		t.Fatalf("rest=%q", rest)
	}
}

func TestExtractXMLFunctionToolCall(t *testing.T) {
	calls, rest := extractToolCallsFromContent(`<function=run_shell>
<parameter=command>
KUBECONFIG=/tmp/config kubectl get pods
</parameter>
</function>
</tool_call>`, map[string]struct{}{"run_shell": {}})
	if len(calls) != 1 || calls[0].Function.Name != "run_shell" || calls[0].Function.Arguments != `{"command":"KUBECONFIG=/tmp/config kubectl get pods"}` {
		t.Fatalf("XML tool call = %#v", calls)
	}
	if rest != "" {
		t.Fatalf("XML tool markup remained: %q", rest)
	}
}

func TestOrdinaryToolNameMentionIsNotRecovered(t *testing.T) {
	known := map[string]struct{}{"str_replace": {}}
	raw := "Use str_replace when an edit is required."
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 0 || rest != raw {
		t.Fatalf("calls=%+v rest=%q", calls, rest)
	}
}

func TestExtractAdjacentNamedToolCalls(t *testing.T) {
	known := map[string]struct{}{"str_replace": {}, "run_tests": {}}
	raw := `str_replace{"path":"calc/calc.go","old":"old","new":"new"}run_tests{"command":"go test ./..."}`
	calls, rest := extractToolCallsFromContent(raw, known)
	if len(calls) != 2 || rest != "" {
		t.Fatalf("calls=%+v rest=%q", calls, rest)
	}
	if calls[0].Function.Name != "str_replace" || calls[1].Function.Name != "run_tests" {
		t.Fatalf("call order/names=%q,%q", calls[0].Function.Name, calls[1].Function.Name)
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
