package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/saurabhahuja71/agenterm/internal/config"
	"github.com/saurabhahuja71/agenterm/internal/tools"
)

func TestHTTPConfigExpandsURLAndBearerTokenWithoutLeakingIt(t *testing.T) {
	t.Setenv("TEST_MCP_URL", "https://mcp.example.test/session")
	t.Setenv("TEST_MCP_TOKEN", "secret-token-value")
	s := config.MCPServer{Name: "zoho", Transport: "streamable_http", URL: "${TEST_MCP_URL}", AuthEnv: "TEST_MCP_TOKEN"}
	endpoint, headers, err := httpConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://mcp.example.test/session" || headers.Get("Authorization") != "Bearer secret-token-value" {
		t.Fatalf("unexpected HTTP config: endpoint=%q authorization=%q", endpoint, headers.Get("Authorization"))
	}
	_, _, unresolvedErr := httpConfig(config.MCPServer{Name: "zoho", URL: "${MISSING_URL}"})
	if errText := unresolvedErr.Error(); strings.Contains(errText, "secret-token-value") {
		t.Fatalf("secret appeared in error: %s", errText)
	}
}

func TestHTTPConfigRejectsMissingURLAndAuthentication(t *testing.T) {
	if err := requireHTTPConfigError(config.MCPServer{Name: "zoho", Transport: "streamable_http"}); err == nil || !strings.Contains(err.Error(), "URL") {
		t.Fatalf("missing URL error = %v", err)
	}
	if err := requireHTTPConfigError(config.MCPServer{Name: "zoho", URL: "https://example.test/mcp", AuthEnv: "MISSING_TOKEN"}); err == nil || !strings.Contains(err.Error(), "MISSING_TOKEN") {
		t.Fatalf("missing auth error = %v", err)
	}
}

func TestStreamableHTTPDiscoveryCallAndError(t *testing.T) {
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	server := mcp.NewServer(&mcp.Implementation{Name: "mock-zoho", Version: "test"}, nil)
	server.AddTool(&mcp.Tool{Name: "inspect", Description: "read-only inspection", InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string"}}}`)}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "scope=" + args["scope"].(string)}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "fail", Description: "mock failure", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "read-only provider failure"}}}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()

	mgr := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Connect(ctx, config.MCPServer{Name: "zoho", Enabled: true, Transport: "streamable_http", URL: httpServer.URL, AuthEnv: "TEST_MCP_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	reg := tools.NewRegistry()
	mgr.RegisterOnto(reg)
	if !contains(reg.Names(), "zoho__inspect") || !contains(reg.Names(), "zoho__fail") {
		t.Fatalf("registered tools = %v", reg.Names())
	}
	out, err := reg.Run(ctx, "zoho__inspect", `{"scope":"crm"}`)
	if err != nil || out != "scope=crm" {
		t.Fatalf("inspect result = %q, err=%v", out, err)
	}
	out, err = reg.Run(ctx, "zoho__fail", `{}`)
	if err == nil || !strings.Contains(out, "read-only provider failure") {
		t.Fatalf("MCP error result = %q, err=%v", out, err)
	}
}

func TestStreamableHTTPConnectHonorsTimeout(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer httpServer.Close()
	mgr := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := mgr.Connect(ctx, config.MCPServer{Name: "slow", URL: httpServer.URL})
	if err == nil {
		t.Fatal("Connect succeeded past context timeout")
	}
}

func requireHTTPConfigError(s config.MCPServer) error {
	_, _, err := httpConfig(s)
	return err
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
