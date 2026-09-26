package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// runtimeTestServer is an in-process MCP server whose tools count calls.
type runtimeTestServer struct {
	server *server.MCPServer
	calls  atomic.Int64
}

func newRuntimeTestServer(t *testing.T, tools map[string]func(mcp.CallToolRequest) *mcp.CallToolResult) *runtimeTestServer {
	t.Helper()
	fake := &runtimeTestServer{server: server.NewMCPServer("fake", "1.0.0", server.WithToolCapabilities(false))}
	for name, handler := range tools {
		fake.server.AddTool(mcp.NewTool(name, mcp.WithDescription("fake "+name), mcp.WithString("service")),
			func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				fake.calls.Add(1)
				return handler(request), nil
			})
	}
	return fake
}

func attachRuntimeTestServer(t *testing.T, manager *MCPToolManager, name string, cfg MCPServerConfig, fake *runtimeTestServer) {
	t.Helper()
	cli, err := client.NewInProcessClient(fake.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachClient(t.Context(), name, cfg, cli); err != nil {
		t.Fatalf("AttachClient(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = manager.Close() })
}

func textResult(text string) func(mcp.CallToolRequest) *mcp.CallToolResult {
	return func(mcp.CallToolRequest) *mcp.CallToolResult { return mcp.NewToolResultText(text) }
}

func allowAll(context.Context, string, string, string) error { return nil }

func descriptorNames(tools []MCPTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestAttachClientRegistersAllowedTools(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"collect_debug": textResult("ok"), "restart": textResult("restarted"),
	})
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{AllowedTools: []string{"collect_debug"}}, fake)
	if got := descriptorNames(manager.SnapshotToolDescriptors()); !slices.Equal(got, []string{"diagnostics__collect_debug"}) {
		t.Fatalf("descriptors = %v", got)
	}
	again, err := client.NewInProcessClient(fake.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachClient(t.Context(), "diagnostics", MCPServerConfig{}, again); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("second attach error = %v", err)
	}
}

func TestReserveRuntimeToolMatchesServerAndNativeName(t *testing.T) {
	manager := NewMCPToolManager("", "")
	for _, name := range []string{"alpha", "beta"} {
		attachRuntimeTestServer(t, manager, name, MCPServerConfig{}, newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
			"probe": textResult(name),
		}))
	}
	manager.mu.Lock()
	manager.loadErrors["broken"] = errors.New("boom")
	manager.mu.Unlock()

	tool, err := manager.ReserveRuntimeTool("alpha", "probe")
	if err != nil || tool.Name != "alpha__probe" || tool.ServerName != "alpha" || tool.OrigName != "probe" {
		t.Fatalf("reserve = %#v, %v", tool, err)
	}
	if again, err := manager.ReserveRuntimeTool("alpha", "probe"); err != nil || again.Name != tool.Name {
		t.Fatalf("second reserve = %#v, %v", again, err)
	}
	if got := descriptorNames(manager.SnapshotToolDescriptors()); !slices.Equal(got, []string{"beta__probe"}) {
		t.Fatalf("descriptors after reserve = %v, want the other server's probe only", got)
	}
	for _, tt := range []struct {
		server, tool, want string
	}{
		{server: "alpha", tool: "missing", want: "does not expose tool"},
		{server: "alpha", tool: "Probe", want: "does not expose tool"},
		{server: "gamma", tool: "probe", want: "is not loaded"},
		{server: "broken", tool: "probe", want: "failed to load: boom"},
	} {
		if _, err := manager.ReserveRuntimeTool(tt.server, tt.tool); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("reserve(%s, %s) error = %v, want %q", tt.server, tt.tool, err, tt.want)
		}
	}
}

func TestLoadToolsRecordsServerLoadErrors(t *testing.T) {
	manager := NewMCPToolManager("", "")
	_ = manager.LoadTools(t.Context(), map[string]MCPServerConfig{"broken": {Type: "bogus"}})
	if _, err := manager.ReserveRuntimeTool("broken", "probe"); err == nil || !strings.Contains(err.Error(), "unsupported MCP server type") {
		t.Fatalf("reserve error = %v, want the server's load error", err)
	}
}

func TestReservedToolsLeaveModelFacingSurfaces(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"collect_debug": textResult("ok"), "list": textResult("listed"),
	})
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, fake)
	reserved, err := manager.ReserveRuntimeTool("diagnostics", "collect_debug")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := MCPToolDescriptorSHA256(reserved)
	if err != nil {
		t.Fatal(err)
	}
	var agentTools []string
	for _, tool := range manager.AsAgentTools() {
		agentTools = append(agentTools, tool.Info().Name)
	}
	if !slices.Equal(agentTools, []string{"diagnostics__list"}) {
		t.Fatalf("agent tools = %v", agentTools)
	}
	if got := descriptorNames(manager.GetTools()); !slices.Equal(got, []string{"diagnostics__list"}) {
		t.Fatalf("GetTools = %v", got)
	}
	ctx := WithToolAuthorizer(t.Context(), allowAll)
	if _, _, err := manager.ExecuteAuthorizedTool(ctx, reserved.Name, digest, `{}`); !IsToolDescriptorMismatchError(err) {
		t.Fatalf("model-facing call of a reserved tool error = %v", err)
	}
	if _, _, err := manager.ExecuteTool(ctx, reserved.Name, `{}`); err == nil {
		t.Fatal("unauthorized model-facing call of a reserved tool succeeded")
	}
	visible, _ := manager.toolMap["diagnostics__list"]
	visibleDigest, err := MCPToolDescriptorSHA256(visible)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExecuteRuntimeTool(t.Context(), visible.Name, visibleDigest, `{}`, allowAll); !IsToolDescriptorMismatchError(err) {
		t.Fatalf("runtime call of an unreserved tool error = %v", err)
	}
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("CallTool count = %d, want 0", calls)
	}
}

func TestExecuteRuntimeToolChecksBeforeTransport(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{"collect_debug": textResult("ok")})
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, fake)
	tool, err := manager.ReserveRuntimeTool("diagnostics", "collect_debug")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := MCPToolDescriptorSHA256(tool)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("denied")
	tests := []struct {
		name      string
		digest    string
		authorize ToolAuthorizer
		wantErr   func(error) bool
	}{
		{name: "missing authorizer", digest: digest, wantErr: IsToolAuthorizationError},
		{name: "missing digest", authorize: allowAll, wantErr: IsToolDescriptorMismatchError},
		{name: "changed digest", digest: strings.Repeat("0", 64), authorize: allowAll, wantErr: IsToolDescriptorMismatchError},
		{name: "denied", digest: digest, authorize: func(context.Context, string, string, string) error { return denied }, wantErr: func(err error) bool {
			return IsToolAuthorizationError(err) && errors.Is(err, denied)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := manager.ExecuteRuntimeTool(t.Context(), tool.Name, tt.digest, `{"service":"api"}`, tt.authorize); !tt.wantErr(err) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("CallTool count after rejected calls = %d, want 0", calls)
	}

	var seen []string
	authorize := func(_ context.Context, server, native, input string) error {
		seen = []string{server, native, input}
		return nil
	}
	result, err := manager.ExecuteRuntimeTool(t.Context(), tool.Name, digest, `{"service":"api"}`, authorize)
	if err != nil {
		t.Fatalf("authorized call: %v", err)
	}
	if !slices.Equal(seen, []string{"diagnostics", "collect_debug", `{"service":"api"}`}) {
		t.Fatalf("authorizer saw %v", seen)
	}
	if calls := fake.calls.Load(); calls != 1 || len(result.Content) != 1 || result.Content[0] != (RuntimeToolContent{Type: "text", Text: "ok"}) {
		t.Fatalf("calls = %d result = %#v", calls, result)
	}
}

func TestExecuteRuntimeToolPreservesResultStructure(t *testing.T) {
	tests := []struct {
		name           string
		result         *mcp.CallToolResult
		wantError      bool
		wantStructured string
		wantContent    []RuntimeToolContent
	}{
		{name: "structured", result: mcp.NewToolResultStructured(map[string]any{"summary": "ok"}, "fallback"),
			wantStructured: `{"summary":"ok"}`, wantContent: []RuntimeToolContent{{Type: "text", Text: "fallback"}}},
		{name: "mixed blocks", result: &mcp.CallToolResult{Content: []mcp.Content{
			mcp.NewTextContent("first"), mcp.NewImageContent("aGk=", "image/png"), mcp.NewTextContent("second"),
		}}, wantContent: []RuntimeToolContent{{Type: "text", Text: "first"}, {Type: "image"}, {Type: "text", Text: "second"}}},
		{name: "tool error", result: mcp.NewToolResultError("boom"), wantError: true, wantContent: []RuntimeToolContent{{Type: "text", Text: "boom"}}},
		{name: "empty", result: &mcp.CallToolResult{}, wantContent: []RuntimeToolContent{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewMCPToolManager("", "")
			attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
				"collect_debug": func(mcp.CallToolRequest) *mcp.CallToolResult { return tt.result },
			}))
			tool, err := manager.ReserveRuntimeTool("diagnostics", "collect_debug")
			if err != nil {
				t.Fatal(err)
			}
			digest, err := MCPToolDescriptorSHA256(tool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := manager.ExecuteRuntimeTool(t.Context(), tool.Name, digest, `{}`, allowAll)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.IsError != tt.wantError {
				t.Fatalf("IsError = %v", result.IsError)
			}
			if tt.wantStructured == "" {
				if len(result.StructuredContent) != 0 {
					t.Fatalf("structured content = %s", result.StructuredContent)
				}
			} else {
				var got any
				if err := json.Unmarshal(result.StructuredContent, &got); err != nil {
					t.Fatalf("structured content %s: %v", result.StructuredContent, err)
				}
				if gotJSON, _ := json.Marshal(got); string(gotJSON) != tt.wantStructured {
					t.Fatalf("structured content = %s, want %s", gotJSON, tt.wantStructured)
				}
			}
			if !slices.Equal(result.Content, tt.wantContent) {
				t.Fatalf("content = %#v, want %#v", result.Content, tt.wantContent)
			}
		})
	}
}

func TestMCPToolCallsSendArgumentNumbersExactly(t *testing.T) {
	const arguments = `{"id":9007199254740993,"ratio":0.1,"nested":{"big":12345678901234567890}}`
	manager := NewMCPToolManager("", "")
	var received []string
	capture := func(request mcp.CallToolRequest) *mcp.CallToolResult {
		raw, _ := request.GetRawArguments().(json.RawMessage)
		received = append(received, string(raw))
		return mcp.NewToolResultText("ok")
	}
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"collect_debug": capture, "list": capture,
	})
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, fake)
	runtimeTool, err := manager.ReserveRuntimeTool("diagnostics", "collect_debug")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDigest, err := MCPToolDescriptorSHA256(runtimeTool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExecuteRuntimeTool(t.Context(), runtimeTool.Name, runtimeDigest, arguments, allowAll); err != nil {
		t.Fatalf("runtime call: %v", err)
	}
	visible := manager.toolMap["diagnostics__list"]
	visibleDigest, err := MCPToolDescriptorSHA256(visible)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithToolAuthorizer(t.Context(), allowAll)
	if _, _, err := manager.ExecuteAuthorizedTool(ctx, visible.Name, visibleDigest, arguments); err != nil {
		t.Fatalf("worker call: %v", err)
	}
	if len(received) != 2 {
		t.Fatalf("server received %d calls, want 2", len(received))
	}
	for index, path := range []string{"runtime", "worker"} {
		for _, want := range []string{"9007199254740993", "0.1", "12345678901234567890"} {
			if !strings.Contains(received[index], want) {
				t.Fatalf("%s call sent %s, want the number %s unchanged", path, received[index], want)
			}
		}
	}

	if _, _, err := manager.ExecuteAuthorizedTool(ctx, visible.Name, visibleDigest, `{"id":1} {"id":2}`); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("arguments with trailing data: error = %v", err)
	}
	if _, _, err := manager.ExecuteAuthorizedTool(ctx, visible.Name, visibleDigest, `["id"]`); err == nil || !strings.Contains(err.Error(), "invalid tool arguments") {
		t.Fatalf("non-object arguments: error = %v", err)
	}
	if calls := fake.calls.Load(); calls != 2 {
		t.Fatalf("CallTool count = %d, want only the two valid calls", calls)
	}
}

func TestAttachClientRegistersEachServerOnce(t *testing.T) {
	manager := NewMCPToolManager("", "")
	t.Cleanup(func() { _ = manager.Close() })
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{"collect_debug": textResult("ok")})
	const attempts = 8
	errs := make(chan error, attempts)
	var ready sync.WaitGroup
	ready.Add(attempts)
	start := make(chan struct{})
	for range attempts {
		cli, err := client.NewInProcessClient(fake.server)
		if err != nil {
			t.Fatal(err)
		}
		if err := cli.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		go func() {
			ready.Done()
			<-start
			errs <- manager.AttachClient(t.Context(), "diagnostics", MCPServerConfig{}, cli)
		}()
	}
	ready.Wait()
	close(start)
	succeeded := 0
	for range attempts {
		if err := <-errs; err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "already loaded") {
			t.Fatalf("attach error = %v", err)
		}
	}
	manager.mu.RLock()
	clients, tools := len(manager.clients), len(manager.tools)
	manager.mu.RUnlock()
	if succeeded != 1 || clients != 1 || tools != 1 {
		t.Fatalf("succeeded=%d clients=%d tools=%d, want one registration without duplicates", succeeded, clients, tools)
	}
}

func TestLoadToolsDoesNotReplaceALoadedServer(t *testing.T) {
	remote := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{"remote_only": textResult("remote")})
	httpServer := httptest.NewServer(server.NewStreamableHTTPServer(remote.server))
	t.Cleanup(httpServer.Close)
	remoteConfig := map[string]MCPServerConfig{"diagnostics": {Type: "remote", URL: httpServer.URL}}

	manager := NewMCPToolManager("", "")
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"collect_debug": textResult("ok"),
	}))
	if err := manager.LoadTools(t.Context(), remoteConfig); err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	if got := descriptorNames(manager.SnapshotToolDescriptors()); !slices.Equal(got, []string{"diagnostics__collect_debug"}) {
		t.Fatalf("descriptors = %v, want the attached server's tools only", got)
	}
	if _, err := manager.ReserveRuntimeTool("diagnostics", "remote_only"); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("reserve of the rejected server's tool error = %v, want its registration conflict", err)
	}

	fresh := NewMCPToolManager("", "")
	t.Cleanup(func() { _ = fresh.Close() })
	fresh.loadErrors["diagnostics"] = errors.New("earlier failure")
	if err := fresh.LoadTools(t.Context(), remoteConfig); err != nil {
		t.Fatalf("LoadTools after a failure: %v", err)
	}
	if _, err := fresh.ReserveRuntimeTool("diagnostics", "missing"); err == nil || strings.Contains(err.Error(), "earlier failure") || !strings.Contains(err.Error(), "does not expose tool") {
		t.Fatalf("reserve error = %v, want the current state instead of the stale load error", err)
	}
}

func TestAttachClientClearsAStaleLoadError(t *testing.T) {
	manager := NewMCPToolManager("", "")
	manager.loadErrors["diagnostics"] = errors.New("earlier failure")
	attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"collect_debug": textResult("ok"),
	}))
	if _, err := manager.ReserveRuntimeTool("diagnostics", "missing"); err == nil || strings.Contains(err.Error(), "earlier failure") || !strings.Contains(err.Error(), "does not expose tool") {
		t.Fatalf("reserve error = %v, want the current state instead of the stale load error", err)
	}
}
