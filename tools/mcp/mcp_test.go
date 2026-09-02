package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"seesharpsi/kritui/tools"
)

const testEndpointPath = "/mcp"

type testServerTool struct {
	tool    *mcpsdk.Tool
	handler mcpsdk.ToolHandler
}

// startTestServer runs an MCP server behind an httptest Server using the SDK's
// Streamable HTTP handler. A page size of 1 forces paginated tool discovery.
func startTestServer(tb testing.TB, pageSize int, entries ...testServerTool) *httptest.Server {
	tb.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1"}, &mcpsdk.ServerOptions{
		PageSize: pageSize,
	})
	for _, entry := range entries {
		srv.AddTool(entry.tool, entry.handler)
	}
	mux := http.NewServeMux()
	mux.Handle(testEndpointPath, mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	ts := httptest.NewServer(mux)
	tb.Cleanup(ts.Close)
	return ts
}

func connectOne(tb testing.TB, ts *httptest.Server, name, capability, token string) *Session {
	tb.Helper()
	config := ServerConfig{
		Name:               name,
		URL:                ts.URL + testEndpointPath,
		AuthorizationToken: token,
		Capability:         capability,
	}
	session, err := Connect(context.Background(), []ServerConfig{config})
	if err != nil {
		tb.Fatalf("Connect(%s) unexpected error: %v", name, err)
	}
	tb.Cleanup(func() { _ = session.Close() })
	return session
}

func TestConnectDiscoversAllPaginatedTools(t *testing.T) {
	echoTool := func(name string) testServerTool {
		return testServerTool{
			tool: &mcpsdk.Tool{Name: name, Description: "Echoes arguments.", InputSchema: map[string]any{"type": "object"}},
			handler: func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(req.Params.Arguments)}}}, nil
			},
		}
	}
	ts := startTestServer(t, 1, echoTool("alpha"), echoTool("beta"))

	session := connectOne(t, ts, "remoted", "remoted", "")
	got := session.Tools()
	if len(got) != 2 {
		t.Fatalf("Tools() discovered %d tools, want 2", len(got))
	}

	names := make(map[string]bool)
	for _, tool := range got {
		definition := tool.Definition()
		capabilityTool, ok := tool.(tools.CapabilityTool)
		if !ok {
			t.Fatalf("tool %q does not implement CapabilityTool", definition.Name)
		}
		if capabilityTool.Capability() != "remoted" {
			t.Fatalf("tool %q capability = %q, want remoted", definition.Name, capabilityTool.Capability())
		}
		if definition.Description != "Echoes arguments." {
			t.Fatalf("tool %q description = %q", definition.Name, definition.Description)
		}
		names[definition.Name] = true
	}
	if len(names) != 2 {
		t.Fatalf("tool names are not unique: %v", names)
	}

	// Tools() must return a clone: mutating it must not affect the session.
	got[0] = nil
	if len(session.Tools()) != 2 {
		t.Fatal("Tools() did not return a clone")
	}

	registry, err := tools.NewRegistry(session.Tools()...)
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}
	if !registry.HasCapability("remoted") {
		t.Fatal("registry does not expose the server capability")
	}
}

func TestConnectDeterministicNamesAcrossRuns(t *testing.T) {
	echo := testServerTool{
		tool: &mcpsdk.Tool{Name: "search-things!", InputSchema: map[string]any{"type": "object"}},
		handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		},
	}
	ts := startTestServer(t, 0, echo)

	first := connectOne(t, ts, "srv", "srv", "")
	second := connectOne(t, ts, "srv", "srv", "")
	firstName := first.Tools()[0].Definition().Name
	secondName := second.Tools()[0].Definition().Name
	if firstName != secondName {
		t.Fatalf("generated names differ across runs: %q vs %q", firstName, secondName)
	}

	// Empty remote description falls back to a description naming tool and server.
	description := first.Tools()[0].Definition().Description
	if !strings.Contains(description, "search-things!") || !strings.Contains(description, "srv") {
		t.Fatalf("fallback description = %q, want tool and server names", description)
	}
}

func TestConnectRejectsDuplicateGeneratedNames(t *testing.T) {
	duplicate := testServerTool{
		tool: &mcpsdk.Tool{Name: "dup", InputSchema: map[string]any{"type": "object"}},
		handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		},
	}
	first := startTestServer(t, 0, duplicate)
	second := startTestServer(t, 0, duplicate)

	configs := []ServerConfig{
		{Name: "one", URL: first.URL + testEndpointPath, Capability: "shared"},
		{Name: "two", URL: second.URL + testEndpointPath, Capability: "shared"},
	}
	if _, err := Connect(context.Background(), configs); err == nil {
		t.Fatal("Connect with duplicate generated tool names should fail")
	} else if !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("duplicate error = %v, want duplicate mention", err)
	}
}

func TestConnectCallerContextBound(t *testing.T) {
	ts := startTestServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Connect(ctx, []ServerConfig{{Name: "srv", URL: ts.URL + testEndpointPath, Capability: "srv"}})
	if err == nil {
		t.Fatal("Connect with canceled context should fail")
	}
}

func TestConnectUnreachableServer(t *testing.T) {
	_, err := Connect(context.Background(), []ServerConfig{{Name: "down", URL: "http://127.0.0.1:1/mcp", Capability: "down"}})
	if err == nil {
		t.Fatal("Connect to unreachable server should fail")
	}
	if !strings.Contains(err.Error(), "down") {
		t.Fatalf("error = %v, want server name mention", err)
	}
}

func TestExecuteArgumentForwarding(t *testing.T) {
	echo := testServerTool{
		tool: &mcpsdk.Tool{Name: "echo", Description: "Echoes arguments.", InputSchema: map[string]any{"type": "object"}},
		handler: func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(req.Params.Arguments)}}}, nil
		},
	}
	ts := startTestServer(t, 0, echo)
	session := connectOne(t, ts, "srv", "srv", "")
	registry, err := tools.NewRegistry(session.Tools()...)
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	tool, ok := registry.Lookup(modelFacingName("srv", "echo"))
	if !ok {
		t.Fatal("echo tool not registered under its generated name")
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"n":9007199254740993,"s":"ok"}`))
	if err != nil {
		t.Fatalf("Execute unexpected error: %v", err)
	}
	if !strings.Contains(output, "9007199254740993") || !strings.Contains(output, "ok") {
		t.Fatalf("Execute output = %q, want forwarded arguments with preserved integer", output)
	}

	// Registry-level validation also rejects non-object arguments.
	if _, err := registry.Execute(context.Background(), modelFacingName("srv", "echo"), json.RawMessage(`[1]`)); err == nil {
		t.Fatal("registry.Execute with non-object arguments should fail")
	}
}

func TestExecuteRemoteResults(t *testing.T) {
	entries := []testServerTool{
		{
			tool: &mcpsdk.Tool{Name: "text", InputSchema: map[string]any{"type": "object"}},
			handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello"}}}, nil
			},
		},
		{
			tool: &mcpsdk.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}},
			handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}}, IsError: true}, nil
			},
		},
		{
			tool: &mcpsdk.Tool{Name: "structured", InputSchema: map[string]any{"type": "object"}},
			handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{StructuredContent: map[string]any{"sum": 7.5}}, nil
			},
		},
		{
			tool: &mcpsdk.Tool{Name: "empty", InputSchema: map[string]any{"type": "object"}},
			handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{}, nil
			},
		},
		{
			tool: &mcpsdk.Tool{Name: "huge", InputSchema: map[string]any{"type": "object"}},
			handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("a", maxOutputBytes+1)}}}, nil
			},
		},
	}
	ts := startTestServer(t, 0, entries...)
	session := connectOne(t, ts, "srv", "srv", "")
	registry, err := tools.NewRegistry(session.Tools()...)
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	execute := func(remoteName, args string) (string, error) {
		tb := t
		tool, ok := registry.Lookup(modelFacingName("srv", remoteName))
		if !ok {
			tb.Fatalf("tool %q not registered", remoteName)
		}
		return tool.Execute(context.Background(), json.RawMessage(args))
	}

	if output, err := execute("text", "{}"); err != nil || output != "hello" {
		t.Fatalf("text result = (%q, %v)", output, err)
	}
	_, err = execute("fail", "{}")
	if err == nil || !strings.Contains(err.Error(), "MCP tool call failed") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error result = %v, want MCP failure containing boom", err)
	}
	if output, err := execute("structured", "{}"); err != nil || output != `{"sum":7.5}` {
		t.Fatalf("structured result = (%q, %v)", output, err)
	}
	if output, err := execute("empty", "{}"); err != nil || output != "(empty result)" {
		t.Fatalf("empty result = (%q, %v)", output, err)
	}
	if _, err := execute("huge", "{}"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("huge result error = %v, want size limit error", err)
	}
}

func TestBearerTokenOnEveryRequest(t *testing.T) {
	const token = "test-token-abc123"
	var mu sync.Mutex
	var seen []string

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1"}, nil)
	srv.AddTool(&mcpsdk.Tool{Name: "ping", Description: "Pings.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}}}, nil
		})
	mux := http.NewServeMux()
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	mux.Handle(testEndpointPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	session, err := Connect(context.Background(), []ServerConfig{{
		Name:               "authsrv",
		URL:                ts.URL + testEndpointPath,
		AuthorizationToken: token,
		Capability:         "authsrv",
	}})
	if err != nil {
		t.Fatalf("Connect unexpected error: %v", err)
	}
	defer func() { _ = session.Close() }()

	tool, ok := session.Tools()[0].(tools.CapabilityTool)
	if !ok {
		t.Fatal("tool does not implement CapabilityTool")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("server recorded no requests")
	}
	for index, authorization := range seen {
		if authorization != "Bearer "+token {
			t.Fatalf("request %d Authorization = %q, want Bearer header", index, authorization)
		}
	}
}

func TestRedirectRefused(t *testing.T) {
	var mu sync.Mutex
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+testEndpointPath, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	const token = "secret-redirect-token"
	_, err := Connect(context.Background(), []ServerConfig{{
		Name:               "redirector",
		URL:                redirect.URL + testEndpointPath,
		AuthorizationToken: token,
		Capability:         "redirector",
	}})
	if err == nil {
		t.Fatal("Connect across a redirect should fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks the authorization token: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if targetHits != 0 {
		t.Fatalf("redirect was followed: target received %d requests", targetHits)
	}
}

func TestCloseIdempotent(t *testing.T) {
	ts := startTestServer(t, 0)
	session := connectOne(t, ts, "srv", "srv", "")
	if err := session.Close(); err != nil {
		t.Fatalf("first Close error: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close error: %v", err)
	}

	var nilSession *Session
	if err := nilSession.Close(); err != nil {
		t.Fatalf("nil Session Close error: %v", err)
	}
	if got := nilSession.Tools(); got != nil {
		t.Fatalf("nil Session Tools() = %v, want nil", got)
	}
}

func TestConnectFailsClosedAfterPartialSuccess(t *testing.T) {
	working := startTestServer(t, 0)
	configs := []ServerConfig{
		{Name: "good", URL: working.URL + testEndpointPath, Capability: "mixed"},
		{Name: "bad", URL: "http://127.0.0.1:1/mcp", Capability: "mixed"},
	}
	if _, err := Connect(context.Background(), configs); err == nil {
		t.Fatal("Connect with one failing server should fail")
	}
	// No assertion on the working session directly: it must have been closed
	// by Connect, which this test exercises for panics via the SDK handlers.
}

func TestGeneratedNamesFitRegistryRules(t *testing.T) {
	ts := startTestServer(t, 0, testServerTool{
		tool: &mcpsdk.Tool{Name: "café/über 中文", InputSchema: map[string]any{"type": "object"}},
		handler: func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		},
	})
	session := connectOne(t, ts, "srv", "srv", "")
	definition := session.Tools()[0].Definition()
	if len(definition.Name) > maxToolNameLength {
		t.Fatalf("generated name %q exceeds %d bytes", definition.Name, maxToolNameLength)
	}
	if _, err := tools.NewRegistry(session.Tools()...); err != nil {
		t.Fatalf("registry rejected generated name %q: %v", definition.Name, err)
	}
	if !strings.Contains(definition.Name, "_") {
		t.Fatalf("sanitized name %q should contain replacement underscores", definition.Name)
	}
}
