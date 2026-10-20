package mcpserver

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/inspect"
)

func TestStdioProcessHelper(t *testing.T) {
	if os.Getenv("HUFU_MCP_TRANSPORT_HELPER") != "1" {
		return
	}
	service := newServer(readerFunc(func(ctx context.Context, _ inspect.Kind, _ string) (*inspect.ScopedObservation, error) {
		_, _ = fmt.Fprintln(os.Stderr, "entered")
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	if err := service.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStdioProcessCancellationDeadlineAndDrain(t *testing.T) {
	c, err := client.NewStdioMCPClient(os.Args[0],
		append(os.Environ(), "HUFU_MCP_TRANSPORT_HELPER=1"), "-test.run=^TestStdioProcessHelper$")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var initialize mcp.InitializeRequest
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "lifecycle-fixture", Version: "1"}
	if _, err := c.Initialize(ctx, initialize); err != nil {
		t.Fatal(err)
	}
	stderr, ok := client.GetStderr(c)
	if !ok {
		t.Fatal("missing helper stderr")
	}
	markers := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			markers <- scanner.Text()
		}
		close(markers)
	}()
	type result struct {
		response *transport.JSONRPCResponse
		err      error
	}
	call := func(id string) <-chan result {
		done := make(chan result, 1)
		go func() {
			response, err := c.GetTransport().SendRequest(ctx, transport.JSONRPCRequest{
				JSONRPC: "2.0", ID: mcp.NewRequestId(id), Method: "tools/call",
				Params: map[string]any{"name": "hufu_run_status", "arguments": map[string]any{}},
			})
			done <- result{response, err}
		}()
		select {
		case marker := <-markers:
			if marker != "entered" {
				t.Fatalf("unexpected marker: %q", marker)
			}
		case <-ctx.Done():
			t.Fatal("handler did not start")
		}
		return done
	}
	checkCancelled := func(done <-chan result) {
		t.Helper()
		select {
		case got := <-done:
			if got.err != nil || got.response == nil {
				t.Fatalf("tool response: %v", got.err)
			}
			var body struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(got.response.Result, &body); err != nil || !body.IsError ||
				len(body.Content) != 1 || body.Content[0].Text != `{"code":"cancelled"}` {
				t.Fatalf("cancellation was not a safe tool error: %#v %v", body, err)
			}
		case <-ctx.Done():
			t.Fatal("handler did not finish")
		}
	}
	var occupied []<-chan result
	for i := range MaxConcurrent {
		occupied = append(occupied, call(fmt.Sprintf("occupied-%d", i)))
	}
	overload, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "hufu_run_status", Arguments: map[string]any{},
	}})
	if err != nil || !overload.IsError || resultText(t, overload) != `{"code":"resource_limit"}` {
		t.Fatalf("stdio overload was not rejected: %#v %v", overload, err)
	}
	for i, done := range occupied {
		if err := c.GetTransport().SendNotification(ctx, mcp.JSONRPCNotification{
			JSONRPC: "2.0", Notification: mcp.Notification{
				Method: "notifications/cancelled",
				Params: mcp.NotificationParams{AdditionalFields: map[string]any{"requestId": fmt.Sprintf("occupied-%d", i)}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		checkCancelled(done)
	}
	cancelled := call("cancel-me")
	if err := c.GetTransport().SendNotification(ctx, mcp.JSONRPCNotification{
		JSONRPC: "2.0", Notification: mcp.Notification{
			Method: "notifications/cancelled",
			Params: mcp.NotificationParams{AdditionalFields: map[string]any{"requestId": "cancel-me"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	checkCancelled(cancelled)
	// Exercise the actual production timeout rather than a fake clock.
	start := time.Now()
	checkCancelled(call("deadline"))
	if elapsed := time.Since(start); elapsed < RequestTimeout || elapsed > RequestTimeout+3*time.Second {
		t.Fatalf("unexpected production deadline: %v", elapsed)
	}
	draining := call("eof")
	start = time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("EOF did not shut down cleanly: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("EOF waited for the request timeout instead of cancelling handlers")
	}
	select {
	case <-draining:
	case <-ctx.Done():
		t.Fatal("client request leaked on shutdown")
	}
}

func TestRequestFrameBound(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &requestReader{source: strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)), cancel: cancel}
	if _, err := reader.Read(make([]byte, MaxRequestBytes+1)); err == nil || ctx.Err() == nil {
		t.Fatal("oversized frame did not terminate input")
	}
}
