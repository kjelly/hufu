package mcpserver

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/inspect"
)

func TestStdioCancellationReleasesBlockedOutput(t *testing.T) {
	input, peerInput := io.Pipe()
	peerOutput, output := io.Pipe()
	defer func() { _ = peerInput.Close() }()
	defer func() { _ = peerOutput.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := newServer(readerFunc(func(context.Context, inspect.Kind, string) (*inspect.ScopedObservation, error) {
		t.Fatal("initialize unexpectedly invoked inspector")
		return nil, nil
	}))
	done := make(chan error, 1)
	blocked := &signalledOutput{WriteCloser: output, entered: make(chan struct{})}
	go func() { done <- s.Serve(ctx, input, blocked) }()
	// The peer never reads the initialize response. Cancellation must
	// release the transport writer rather than waiting for peer cooperation.
	if _, err := io.WriteString(peerInput, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"clientInfo\":{\"name\":\"fixture\",\"version\":\"1\"}}}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("transport did not attempt a response")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked output prevented shutdown")
	}
}

type signalledOutput struct {
	io.WriteCloser
	entered chan struct{}
	once    sync.Once
}

func (w *signalledOutput) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.WriteCloser.Write(p)
}

type readerFunc func(context.Context, inspect.Kind, string) (*inspect.ScopedObservation, error)

func (f readerFunc) Inspect(ctx context.Context, kind inspect.Kind, task string) (*inspect.ScopedObservation, error) {
	return f(ctx, kind, task)
}

func testRequest(raw string) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{RawArguments: []byte(raw)}}
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("invalid tool result: %#v", result)
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content: %#v", result.Content)
	}
	return text.Text
}

func TestToolArgumentsFailClosed(t *testing.T) {
	for _, raw := range []string{
		`{"workspace":"/private"}`, `{"task_id":null}`, `{"task_id":""}`,
		`{"task_id":1}`, `[]`, `null`, `{"task_id":"one","task_id":"two"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s := newServer(readerFunc(func(context.Context, inspect.Kind, string) (*inspect.ScopedObservation, error) {
				t.Fatal("invalid argument reached inspector")
				return nil, nil
			}))
			result, err := s.handler(inspect.KindRoutes)(t.Context(), testRequest(raw))
			if err != nil || !result.IsError || resultText(t, result) != `{"code":"invalid_argument"}` {
				t.Fatalf("invalid argument accepted: %#v %v", result, err)
			}
		})
	}
	s := newServer(readerFunc(func(_ context.Context, kind inspect.Kind, task string) (*inspect.ScopedObservation, error) {
		if kind != inspect.KindRoutes || task != "task-1" {
			t.Fatalf("wrong arguments: %s %s", kind, task)
		}
		return &inspect.ScopedObservation{}, nil
	}))
	result, err := s.handler(inspect.KindRoutes)(t.Context(), testRequest(`{"task_id":"task-1"}`))
	if err != nil || result.IsError {
		t.Fatalf("valid argument rejected: %#v %v", result, err)
	}
	result, err = s.handler(inspect.KindRun)(t.Context(), testRequest(`{"task_id":"task-1"}`))
	if err != nil || !result.IsError {
		t.Fatal("status accepted task filter")
	}
}

func TestToolDeadlineAndOverload(t *testing.T) {
	entered := make(chan struct{}, MaxConcurrent)
	s := newServer(readerFunc(func(ctx context.Context, _ inspect.Kind, _ string) (*inspect.ScopedObservation, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var workers sync.WaitGroup
	for range MaxConcurrent {
		workers.Go(func() {
			result, err := s.handler(inspect.KindRun)(ctx, testRequest("{}"))
			if err != nil || resultText(t, result) != `{"code":"cancelled"}` {
				t.Errorf("cancelled request: %#v %v", result, err)
			}
		})
	}
	for range MaxConcurrent {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	result, err := s.handler(inspect.KindRun)(t.Context(), testRequest("{}"))
	if err != nil || resultText(t, result) != `{"code":"resource_limit"}` {
		t.Fatalf("overload queued or accepted: %#v %v", result, err)
	}
	cancel()
	workers.Wait()
	s.timeout = time.Millisecond
	result, err = s.handler(inspect.KindRun)(t.Context(), testRequest("{}"))
	if err != nil || resultText(t, result) != `{"code":"cancelled"}` {
		t.Fatalf("deadline lost: %#v %v", result, err)
	}
}

func TestToolErrorsAndOutputAreBounded(t *testing.T) {
	s := newServer(readerFunc(func(context.Context, inspect.Kind, string) (*inspect.ScopedObservation, error) {
		return nil, errors.New("secret path /private/token")
	}))
	result, err := s.handler(inspect.KindRun)(t.Context(), testRequest("{}"))
	if err != nil || resultText(t, result) != `{"code":"internal_error"}` {
		t.Fatalf("raw error escaped: %#v %v", result, err)
	}
	var buffer limitedBuffer
	if _, err := buffer.Write(make([]byte, MaxResponseBytes+1)); err == nil || buffer.buffer.Len() != 0 {
		t.Fatal("oversized serialization admitted")
	}
	// Escaping the textual JSON fallback can make the SDK's wire response
	// larger than the projection itself. Measure and replace that full frame.
	raw, err := json.Marshal(mcp.JSONRPCResponse{
		JSONRPC: "2.0", ID: mcp.NewRequestId("request-1"),
		Result: mcp.NewToolResultText(strings.Repeat(`"`, MaxResponseBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err := (responseWriter{&wire}).Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if wire.Len() > MaxResponseBytes || !strings.Contains(wire.String(), "resource_limit") ||
		!strings.Contains(wire.String(), "request-1") {
		t.Fatalf("wire bound failed: %d bytes", wire.Len())
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire.Bytes(), &decoded); err != nil {
		t.Fatalf("truncated protocol frame: %v", err)
	}
}
