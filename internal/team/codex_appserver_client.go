package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// This file implements a generic JSON-RPC 2.0 client over stdio
// (docs/hufu-external-coding-agent-runtime-spec.md §12, PR-08). It knows
// nothing about Codex-specific methods — codex_appserver_protocol.go builds
// the typed thread/turn vocabulary on top of it.
//
// Wire framing assumption: one JSON-RPC message per line (newline-delimited
// JSON), matching common JSON-RPC-over-stdio CLI conventions. spec.md does
// not pin an exact byte-level framing for "codex app-server"; this is a
// documented assumption for the fake-server fixtures this phase tests
// against, to be confirmed against the real codex binary by the opt-in smoke
// tests (§38) before Phase 5 relies on it in production.

// defaultCodexMaxFrameBytes bounds a single JSON-RPC line (§17.2: "A single
// oversized JSON-RPC frame MUST terminate the provider as a protocol
// violation").
const defaultCodexMaxFrameBytes = 16 << 20 // 16 MiB

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// jsonRPCEnvelope decodes any incoming line generically enough to classify
// it: a response has "id" and no "method"; a notification has "method" and
// no "id". A message with both (a server-to-client request) is tolerated and
// ignored — Hufu's driver has no v1 use case that requires answering one,
// since approvalPolicy=never eliminates the only realistic reason Codex
// would ask the client something (§13.3, §15).
type jsonRPCEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *jsonRPCError   `json:"error,omitempty"`
}

// CodexNotification is one server-initiated notification, generically
// decoded. Callers interested in a specific method (e.g. "turn/completed")
// filter by Method themselves; every other notification is bounded,
// tolerated, and otherwise ignored (§13.3).
type CodexNotification struct {
	Method string
	Params json.RawMessage
}

// CodexRPCClient is a generic JSON-RPC 2.0 client over a pair of stdio-style
// streams. One client corresponds to one app-server process
// (§13.1: "one app-server process per Hufu attempt").
type CodexRPCClient struct {
	stdin io.WriteCloser

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan jsonRPCResponse

	closed    bool
	closeErr  error
	closeOnce sync.Once
	closeCh   chan struct{}

	notifications chan CodexNotification
}

type jsonRPCResponse struct {
	Result json.RawMessage
	Error  *jsonRPCError
}

// NewCodexRPCClient wraps an already-started process's stdin/stdout.
// maxFrameBytes <= 0 uses defaultCodexMaxFrameBytes.
func NewCodexRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrameBytes int) *CodexRPCClient {
	if maxFrameBytes <= 0 {
		maxFrameBytes = defaultCodexMaxFrameBytes
	}
	c := &CodexRPCClient{
		stdin:         stdin,
		pending:       make(map[int64]chan jsonRPCResponse),
		closeCh:       make(chan struct{}),
		notifications: make(chan CodexNotification, 1024),
	}
	go c.readLoop(stdout, maxFrameBytes)
	return c
}

// Notifications returns the channel of server-initiated notifications.
func (c *CodexRPCClient) Notifications() <-chan CodexNotification { return c.notifications }

// Done is closed once the client can no longer serve requests (process
// exited, transport error, or Close was called). Callers waiting on
// Notifications() should always also select on Done() to avoid blocking
// forever after a crash.
func (c *CodexRPCClient) Done() <-chan struct{} { return c.closeCh }

// Err returns the reason the client closed, once Done() is closed.
func (c *CodexRPCClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Close disconnects the client without treating it as a transport failure.
func (c *CodexRPCClient) Close() error {
	c.closeWithError(fmt.Errorf("codex rpc client closed"))
	return c.stdin.Close()
}

func (c *CodexRPCClient) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeErr = err
		pending := c.pending
		c.pending = nil
		c.mu.Unlock()
		close(c.closeCh)
		_ = pending // pending callers observe closure via the closeCh select case in Call, not via their channel.
	})
}

// Call sends a JSON-RPC request and blocks for its response, honoring ctx
// cancellation and client closure. result may be nil to discard the result.
func (c *CodexRPCClient) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("codex rpc client is closed")
		}
		return fmt.Errorf("codex %s: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan jsonRPCResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		c.removePending(id)
		return fmt.Errorf("marshal %s params: %w", method, err)
	}
	req := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: paramsJSON}
	line, err := json.Marshal(req)
	if err != nil {
		c.removePending(id)
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	line = append(line, '\n')

	c.mu.Lock()
	_, writeErr := c.stdin.Write(line)
	c.mu.Unlock()
	if writeErr != nil {
		c.removePending(id)
		return fmt.Errorf("write %s request: %w", method, writeErr)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("codex %s: %w", method, resp.Error)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-c.closeCh:
		c.removePending(id)
		err := c.Err()
		if err == nil {
			err = fmt.Errorf("codex rpc client closed")
		}
		return fmt.Errorf("codex %s: %w", method, err)
	}
}

func (c *CodexRPCClient) removePending(id int64) {
	c.mu.Lock()
	if c.pending != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *CodexRPCClient) deliverResponse(id int64, resp jsonRPCResponse) bool {
	c.mu.Lock()
	var ch chan jsonRPCResponse
	if c.pending != nil {
		ch = c.pending[id]
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ch != nil {
		ch <- resp
		return true
	}
	return false
}

// dispatchNotification is always run in its own goroutine so a slow or
// entirely absent consumer of Notifications() can never block readLoop from
// reading the next frame (PR-08: "concurrent notification dispatch").
func (c *CodexRPCClient) dispatchNotification(notif CodexNotification) {
	select {
	case c.notifications <- notif:
	case <-c.closeCh:
	}
}

func (c *CodexRPCClient) readLoop(stdout io.Reader, maxFrameBytes int) {
	scanner := bufio.NewScanner(stdout)
	initial := 64 * 1024
	if initial > maxFrameBytes {
		initial = maxFrameBytes
	}
	scanner.Buffer(make([]byte, 0, initial), maxFrameBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope jsonRPCEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			c.closeWithError(fmt.Errorf("codex app-server sent a malformed JSON-RPC frame: %w", err))
			return
		}
		switch {
		case len(envelope.ID) > 0 && envelope.Method == "":
			var id int64
			if err := json.Unmarshal(envelope.ID, &id); err != nil || id <= 0 {
				if err == nil {
					err = fmt.Errorf("response id must be a positive integer, got %s", string(envelope.ID))
				}
				c.closeWithError(fmt.Errorf("codex app-server sent a response with an invalid id: %w", err))
				return
			}
			if !c.deliverResponse(id, jsonRPCResponse{Result: envelope.Result, Error: envelope.Error}) {
				c.closeWithError(fmt.Errorf("codex app-server sent a response for unknown request id %d", id))
				return
			}
		case envelope.Method != "" && len(envelope.ID) == 0:
			go c.dispatchNotification(CodexNotification{Method: envelope.Method, Params: envelope.Params})
		default:
			// Unrecognized shape (including a server-to-client request):
			// bounded by maxFrameBytes above, tolerated, never crashes the
			// client (§13.3).
		}
	}
	if err := scanner.Err(); err != nil {
		c.closeWithError(fmt.Errorf("codex app-server transport error: %w", err))
		return
	}
	c.closeWithError(fmt.Errorf("codex app-server process closed its output stream"))
}
