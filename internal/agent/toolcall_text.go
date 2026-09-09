package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/saurabhahuja71/agenterm/internal/llm"
)

// Many Ollama models (e.g. some Qwen builds) print tool invocations as plain
// text JSON instead of OpenAI tool_calls. Recover those so the agent loop runs.

var (
	reJSONObject = regexp.MustCompile(`(?s)\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}`)
	reFencedJSON = regexp.MustCompile("(?s)```(?:json|tool)?\\s*(\\{.*?\\})\\s*```")
)

// hasUnsupportedToolMarkup identifies common template tags that are not
// OpenAI-compatible tool_calls. They are retried once, but never executed as
// shell or treated as a completed coding task.
func hasUnsupportedToolMarkup(content string) bool {
	low := strings.ToLower(content)
	return (strings.Contains(low, "<function=") && strings.Contains(low, "</function>")) ||
		strings.Contains(low, "</tool_call>") || strings.Contains(low, "<tool_call>")
}

type textToolCall struct {
	Name      string          `json:"name"`
	Tool      string          `json:"tool"`
	Function  string          `json:"function"`
	Arguments json.RawMessage `json:"arguments"`
	Params    json.RawMessage `json:"parameters"`
	Args      json.RawMessage `json:"args"`
	Path      string          `json:"path"` // sometimes flattened
}

// extractToolCallsFromContent parses pseudo tool-calls from assistant text.
// Returns recovered calls and remaining non-tool text (may be empty).
func extractToolCallsFromContent(content string, knownTools map[string]struct{}) ([]llm.ToolCall, string) {
	content = strings.TrimSpace(content)
	if content == "" || knownTools == nil || len(knownTools) == 0 {
		return nil, content
	}

	var calls []llm.ToolCall
	rest := content

	// Prefer fenced blocks first.
	if locs := reFencedJSON.FindAllStringSubmatchIndex(content, -1); len(locs) > 0 {
		var b strings.Builder
		last := 0
		for _, loc := range locs {
			// loc: full start, full end, group1 start, group1 end
			if len(loc) < 4 {
				continue
			}
			b.WriteString(content[last:loc[0]])
			raw := content[loc[2]:loc[3]]
			if tc, ok := parseOneToolJSON(raw, knownTools); ok {
				calls = append(calls, tc)
			} else {
				b.WriteString(content[loc[0]:loc[1]])
			}
			last = loc[1]
		}
		b.WriteString(content[last:])
		rest = strings.TrimSpace(b.String())
		if len(calls) > 0 {
			return calls, rest
		}
	}

	// Whole message is a single JSON tool call.
	if tc, ok := parseOneToolJSON(content, knownTools); ok {
		return []llm.ToolCall{tc}, ""
	}

	// Scan for JSON objects that look like tool calls.
	matches := reJSONObject.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		return nil, content
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		raw := content[m[0]:m[1]]
		if tc, ok := parseOneToolJSON(raw, knownTools); ok {
			b.WriteString(content[last:m[0]])
			calls = append(calls, tc)
			last = m[1]
			continue
		}
	}
	b.WriteString(content[last:])
	rest = strings.TrimSpace(b.String())
	if len(calls) == 0 {
		return nil, content
	}
	return calls, rest
}

func parseOneToolJSON(raw string, knownTools map[string]struct{}) (llm.ToolCall, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return llm.ToolCall{}, false
	}
	var t textToolCall
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return llm.ToolCall{}, false
	}
	name := firstNonEmpty(t.Name, t.Tool, t.Function)
	name = strings.TrimSpace(name)
	if name == "" {
		return llm.ToolCall{}, false
	}
	// Normalize common aliases
	switch name {
	case "read", "Read", "ReadFile", "read-file":
		name = "read_file"
	case "list", "List", "ListDir", "list-dir", "ls":
		name = "list_dir"
	case "write", "Write", "WriteFile", "write-file":
		name = "write_file"
	case "str_replace", "replace", "Replace", "edit", "Edit", "edit_file", "search_replace":
		name = "str_replace"
	case "git", "Git":
		name = "git"
	case "find", "Find", "FindFiles", "find-files", "search":
		name = "find_files"
	case "grep", "Grep", "search_code", "rg":
		name = "grep"
	case "run_tests", "test", "tests":
		name = "run_tests"
	case "shell", "bash", "run", "run_shell", "execute", "exec", "curl", "wget":
		// curl/wget as tool names → run_shell (command filled below if only url given)
		name = "run_shell"
	case "fetch", "http_get", "httpget", "download":
		name = "fetch"
	}
	if _, ok := knownTools[name]; !ok {
		return llm.ToolCall{}, false
	}

	argsRaw := firstRaw(t.Arguments, t.Params, t.Args)
	args := "{}"
	if len(argsRaw) > 0 {
		args = string(argsRaw)
		// arguments sometimes double-encoded as a string
		var asStr string
		if err := json.Unmarshal(argsRaw, &asStr); err == nil && strings.TrimSpace(asStr) != "" {
			args = asStr
		}
	} else if t.Path != "" {
		b, _ := json.Marshal(map[string]string{"path": t.Path})
		args = string(b)
	}
	// curl/wget-style calls often pass url without wrapping a bash command
	if name == "run_shell" || name == "fetch" {
		args = normalizeFetchOrShellArgs(name, args, t)
	}
	// Must look like JSON object for tool runners
	if !json.Valid([]byte(args)) {
		// wrap as path-only if bare string path
		b, _ := json.Marshal(map[string]string{"path": strings.Trim(args, `"`)})
		args = string(b)
	}

	id := fmt.Sprintf("textcall_%d", time.Now().UnixNano())
	return llm.ToolCall{
		ID:   id,
		Type: "function",
		Function: llm.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}, true
}

// normalizeFetchOrShellArgs fills command/url when models emit curl/wget-shaped JSON.
func normalizeFetchOrShellArgs(name, args string, t textToolCall) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || m == nil {
		m = map[string]any{}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch x := v.(type) {
				case string:
					if strings.TrimSpace(x) != "" {
						return strings.TrimSpace(x)
					}
				}
			}
		}
		return ""
	}
	url := str("url", "URL", "uri", "href")
	if url == "" && strings.HasPrefix(strings.TrimSpace(t.Path), "http") {
		url = strings.TrimSpace(t.Path)
	}
	cmd := str("command", "cmd", "script")

	switch name {
	case "fetch":
		if str("url") == "" && url != "" {
			m["url"] = url
			b, _ := json.Marshal(m)
			return string(b)
		}
	case "run_shell":
		if cmd == "" && url != "" {
			// Prefer curl; fall back to wget if someone only has that (runtime).
			quoted := "'" + strings.ReplaceAll(url, "'", `'\''`) + "'"
			m["command"] = "curl -fsSL " + quoted + " || wget -qO- " + quoted
			b, _ := json.Marshal(m)
			return string(b)
		}
		// Bare script path: "script.sh" without command key
		if cmd == "" {
			if p := str("path", "file", "script_path"); p != "" && strings.HasSuffix(p, ".sh") {
				m["command"] = "bash " + p
				b, _ := json.Marshal(m)
				return string(b)
			}
		}
	}
	return args
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstRaw(parts ...json.RawMessage) json.RawMessage {
	for _, p := range parts {
		if len(p) > 0 && string(p) != "null" {
			return p
		}
	}
	return nil
}

func toolNameSet(reg interface{ Names() []string }) map[string]struct{} {
	if reg == nil {
		return nil
	}
	names := reg.Names()
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// normalizeToolName repairs a provider quirk where streamed function-name
// fragments can contain two adjacent known names (for example,
// "list_dirread_file"). The tool-call arguments remain authoritative; using
// the first known name lets the agent execute the call and continue instead of
// failing the whole turn with an unknown-tool error.
func normalizeToolName(name string, known map[string]struct{}) string {
	name = strings.TrimSpace(name)
	if _, ok := known[name]; ok {
		return name
	}
	best := ""
	for candidate := range known {
		if strings.HasPrefix(name, candidate) && len(candidate) > len(best) {
			best = candidate
		}
	}
	if best != "" {
		return best
	}
	return name
}
