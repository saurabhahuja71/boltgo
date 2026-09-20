package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/llm"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

// Manager holds open MCP sessions and exposes them as tools.Runner entries.
type Manager struct {
	mu       sync.Mutex
	sessions []*session
}

type session struct {
	name    string
	client  *mcp.Client
	session *mcp.ClientSession
	// tool name (prefixed) -> remote tool name
	tools map[string]string
}

func NewManager() *Manager {
	return &Manager{}
}

// ConnectAll connects enabled MCP servers from config.
func (m *Manager) ConnectAll(ctx context.Context, servers []config.MCPServer) error {
	var errs []string
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if err := m.Connect(ctx, s); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", s.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mcp: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *Manager) Connect(ctx context.Context, s config.MCPServer) error {
	name := s.Name
	if name == "" {
		name = "mcp"
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agenterm", Version: "1.0.0"}, nil)

	var transport mcp.Transport
	switch {
	case s.URL != "":
		endpoint, headers, err := httpConfig(s)
		if err != nil {
			return err
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}},
		}
	case s.Command != "":
		if s.Transport != "" && !strings.EqualFold(s.Transport, "stdio") {
			return fmt.Errorf("server %q: command transport must be stdio", name)
		}
		cmd := exec.Command(s.Command, s.Args...)
		transport = &mcp.CommandTransport{Command: cmd}
	default:
		return fmt.Errorf("server %q needs url or command", name)
	}

	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}

	toolMap := map[string]string{}
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			_ = sess.Close()
			return err
		}
		prefixed := name + "__" + t.Name
		toolMap[prefixed] = t.Name
	}

	m.mu.Lock()
	m.sessions = append(m.sessions, &session{
		name:    name,
		client:  client,
		session: sess,
		tools:   toolMap,
	})
	m.mu.Unlock()
	return nil
}

// httpConfig resolves non-secret configuration. AuthEnv is intentionally read
// at connection time so tokens never enter config files or diagnostics.
func httpConfig(s config.MCPServer) (string, http.Header, error) {
	if s.Transport != "" && !strings.EqualFold(s.Transport, "streamable_http") {
		return "", nil, fmt.Errorf("server %q: unsupported URL transport %q", s.Name, s.Transport)
	}
	endpoint := os.ExpandEnv(strings.TrimSpace(s.URL))
	if endpoint == "" || strings.Contains(endpoint, "${") {
		return "", nil, fmt.Errorf("server %q: streamable_http URL is missing or unresolved", s.Name)
	}
	headers := make(http.Header)
	for key, value := range s.Headers {
		value = os.ExpandEnv(value)
		if strings.Contains(value, "${") {
			return "", nil, fmt.Errorf("server %q: header %q contains an unresolved environment variable", s.Name, key)
		}
		headers.Set(key, value)
	}
	if s.AuthEnv != "" {
		token := strings.TrimSpace(os.Getenv(s.AuthEnv))
		if token == "" {
			return "", nil, fmt.Errorf("server %q: bearer token environment variable %q is not set", s.Name, s.AuthEnv)
		}
		headers.Set("Authorization", "Bearer "+token)
	}
	return endpoint, headers, nil
}

type headerTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for key, values := range t.headers {
		clone.Header.Del(key)
		for _, value := range values {
			clone.Header.Add(key, value)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// RegisterOnto adds MCP tools into a tools.Registry.
func (m *Manager) RegisterOnto(reg *tools.Registry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		// need schemas — list tools again is heavy; store schemas at connect
		// Re-list via session for schema
		s := s
		ctx := context.Background()
		for t, err := range s.session.Tools(ctx, nil) {
			if err != nil {
				continue
			}
			prefixed := s.name + "__" + t.Name
			// Capture schema as map
			var params map[string]any
			if t.InputSchema != nil {
				b, _ := json.Marshal(t.InputSchema)
				_ = json.Unmarshal(b, &params)
			}
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			reg.Register(&mcpRunner{
				mgrName:    s.name,
				localName:  prefixed,
				remoteName: t.Name,
				desc:       t.Description,
				schema:     params,
				sess:       s.session,
			})
		}
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		_ = s.session.Close()
	}
	m.sessions = nil
}

func (m *Manager) LLMTools() []llm.Tool {
	// unused if registered into tools.Registry
	return nil
}

type mcpRunner struct {
	mgrName    string
	localName  string
	remoteName string
	desc       string
	schema     map[string]any
	sess       *mcp.ClientSession
}

func (r *mcpRunner) Name() string        { return r.localName }
func (r *mcpRunner) Description() string { return r.desc }
func (r *mcpRunner) Schema() map[string]any {
	return r.schema
}

func (r *mcpRunner) Run(ctx context.Context, argsJSON string) (string, error) {
	var args any
	if strings.TrimSpace(argsJSON) == "" {
		args = map[string]any{}
	} else {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool args: %w", err)
		}
	}
	res, err := r.sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      r.remoteName,
		Arguments: args,
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return formatMCPContent(res), fmt.Errorf("tool error")
	}
	return formatMCPContent(res), nil
}

func formatMCPContent(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
		default:
			raw, _ := json.Marshal(c)
			b.Write(raw)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}
