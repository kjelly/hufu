package mcpserver

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/kjelly/hufu/internal/team"
)

// Serve owns input until shutdown. Closing it on cancellation releases the
// SDK's pending reader; EOF cancels handlers before the SDK drains its workers.
func (s *Server) Serve(ctx context.Context, input io.ReadCloser, output io.WriteCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	closeInput := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer closeInput()
	defer func() { _ = input.Close() }()
	// A peer that stops reading must not trap the SDK in a pipe write
	// while Listen waits to drain workers on EOF or a signal.
	closeOutput := context.AfterFunc(ctx, func() { _ = output.Close() })
	defer closeOutput()
	transport := server.NewStdioServer(s.sdk)
	// One extra transport worker can reject overload while four inspectors
	// are busy. The one-element transport queue is bounded; no inspection
	// waits for admission, and SDK synchronous overflow also hits the gate.
	server.WithWorkerPoolSize(MaxConcurrent + 1)(transport)
	server.WithQueueSize(1)(transport)
	// The caller receives a sanitized error. SDK diagnostics may contain
	// client-supplied values, so do not send its raw logger to stderr.
	server.WithErrorLogger(log.New(io.Discard, "", 0))(transport)
	err := transport.Listen(ctx, &requestReader{source: input, cancel: cancel}, responseWriter{output})
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type requestReader struct {
	source io.Reader
	cancel context.CancelFunc
	size   int
}

func (r *requestReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	for _, value := range p[:n] {
		r.size++
		if r.size > MaxRequestBytes {
			r.cancel()
			return 0, team.ErrReadLimit
		}
		if value == '\n' {
			r.size = 0
		}
	}
	if err != nil {
		r.cancel()
	}
	return n, err
}

// The SDK emits each JSON-RPC response in one formatted write. Check the
// actual wire bytes, including escaped text and request ID, before forwarding.
// Oversize tool results become a small, valid SDK JSON-RPC tool-error response.
type responseWriter struct{ destination io.Writer }

func (w responseWriter) Write(p []byte) (int, error) {
	wire := p
	if len(p) > MaxResponseBytes {
		var response struct {
			ID mcp.RequestId `json:"id"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(p), &response); err != nil {
			return 0, team.ErrReadLimit
		}
		replacement, err := json.Marshal(mcp.JSONRPCResponse{
			JSONRPC: "2.0", ID: response.ID, Result: failure("resource_limit"),
		})
		if err != nil || len(replacement)+1 > MaxResponseBytes {
			return 0, team.ErrReadLimit
		}
		wire = append(replacement, '\n')
	}
	n, err := w.destination.Write(wire)
	if err != nil {
		return 0, err
	}
	if n != len(wire) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
