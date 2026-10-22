package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/processutil"
	"github.com/kjelly/hufu/internal/team"
)

func TestMCPServeProcessHelper(t *testing.T) {
	if os.Getenv("HUFU_MCP_PROCESS_HELPER") != "1" {
		return
	}
	os.Args = []string{"hufu", "mcp", "serve", "--workspace", os.Getenv("HUFU_MCP_WORKSPACE"),
		"--session", "session-cli-inspect", "--branch", "main", "--run", "run-cli-inspect"}
	main()
}

type mcpWireCapture struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *mcpWireCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *mcpWireCapture) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type mcpProcessFixture struct {
	client *client.Client
	cmd    *exec.Cmd
	done   <-chan error
	wire   *mcpWireCapture
	stderr *bytes.Buffer
}

func startMCPProcessFixture(t *testing.T, workspace string) mcpProcessFixture {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMCPServeProcessHelper$")
	command.Env = append(os.Environ(), "HUFU_MCP_PROCESS_HELPER=1", "HUFU_MCP_WORKSPACE="+workspace)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	done, err := processutil.StartAndWait(command)
	if err != nil {
		t.Fatal(err)
	}
	wire := &mcpWireCapture{}
	connection := transport.NewIO(io.TeeReader(stdout, wire), stdin, nil)
	if err := connection.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := client.NewClient(connection)
	t.Cleanup(func() {
		_ = c.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var initialize mcp.InitializeRequest
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "fixture", Version: "1"}
	if _, err := c.Initialize(ctx, initialize); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return mcpProcessFixture{c, command, done, wire, stderr}
}

func callMCPFixture(t *testing.T, c *client.Client, name string, args map[string]any, wantError bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	if err != nil || result == nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError != wantError || len(result.Content) != 1 {
		t.Fatalf("%s: unexpected result %#v", name, result)
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("%s: unexpected content", name)
	}
	var value any
	if err := json.Unmarshal([]byte(content.Text), &value); err != nil {
		t.Fatalf("%s: invalid result JSON: %v", name, err)
	}
	return content.Text
}

func stopMCPProcessFixture(t *testing.T, process mcpProcessFixture, signal bool) {
	t.Helper()
	if signal {
		if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := process.client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("server shutdown: %v; stderr=%s", err, process.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not drain on EOF/signal")
	}
	for line := range strings.SplitSeq(strings.TrimSpace(process.wire.text()), "\n") {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil || frame["jsonrpc"] != "2.0" {
			t.Fatalf("non-protocol stdout frame: %q (%v)", line, err)
		}
		if len(line)+1 > 1<<20 {
			t.Fatalf("oversized wire response: %d", len(line)+1)
		}
	}
}

func TestMCPServeSDKProcessFixedScope(t *testing.T) {
	workspace, _, _ := buildInspectCommandFixture(t)
	otherWorkspace, _, _ := buildInspectCommandFixture(t)
	first := startMCPProcessFixture(t, workspace)
	second := startMCPProcessFixture(t, otherWorkspace)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tools, err := first.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools/list: %#v %v", tools, err)
	}
	for _, name := range []string{"hufu_run_status", "hufu_run_routes", "hufu_run_economics", "hufu_capabilities"} {
		text := callMCPFixture(t, first.client, name, map[string]any{}, false)
		for _, secret := range []string{"sk-fixture-secret", "provider.example", "rawOutput", workspace} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked %q", name, secret)
			}
		}
	}
	original := callMCPFixture(t, first.client, "hufu_run_routes", map[string]any{}, false)
	otherOriginal := callMCPFixture(t, second.client, "hufu_run_routes", map[string]any{}, false)
	var observation struct {
		SourceHead cost.Freshness `json:"source_head"`
	}
	if err := json.Unmarshal([]byte(original), &observation); err != nil {
		t.Fatal(err)
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree.Branches["other"] = &team.SessionBranch{ID: "other", Name: "other", ParentID: "main", ForkEventID: observation.SourceHead.EventID}
	tree.ActiveBranch = "other"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	if after := callMCPFixture(t, first.client, "hufu_run_routes", map[string]any{}, false); after != original {
		t.Fatal("server followed active branch checkout")
	}
	if after := callMCPFixture(t, second.client, "hufu_run_routes", map[string]any{}, false); after != otherOriginal {
		t.Fatal("second server scope changed")
	}
	for _, args := range []map[string]any{{"workspace": otherWorkspace}, {"run": "other"}, {"all_branches": true}} {
		if text := callMCPFixture(t, first.client, "hufu_run_routes", args, true); !strings.Contains(text, "invalid_argument") {
			t.Fatalf("scope override not rejected: %s", text)
		}
	}
	if text := callMCPFixture(t, first.client, "hufu_run_routes", map[string]any{"task_id": "other-task"}, true); !strings.Contains(text, "not_found") {
		t.Fatalf("foreign task not rejected: %s", text)
	}
	stopMCPProcessFixture(t, first, false)
	stopMCPProcessFixture(t, second, true)
}
