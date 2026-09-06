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
