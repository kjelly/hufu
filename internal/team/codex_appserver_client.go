package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/kjelly/hufu/internal/utils"
)

// This file implements the JSON-RPC 2.0 transport over stdio
// (docs/architecture/execution-runtime.md). The typed thread and turn protocol
// lives in codex_appserver_protocol.go.
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

const codexMaxAdvisoryQueueBytes = 4 << 20

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`

	codeRedacted bool
}

func (e *jsonRPCError) Error() string {
	if e.codeRedacted {
		return "code [REDACTED]: " + e.Message
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

func (e *jsonRPCError) redacted(diagnostics *codexTurnDiagnostics) *jsonRPCError {
	if diagnostics == nil {
		diagnostics = newCodexTurnDiagnostics(nil)
	}
	diagnostics.learn(string(e.Data))
	diagnostics.learn(e.Message)
	message := "[invalid provider JSON]"
	if !diagnostics.unsafe {
		message = utils.TruncateRunes(redactCodexProviderText(e.Message, diagnostics.secrets), codexFailureEvidenceMaxRunes)
	}
	code := strconv.Itoa(e.Code)
	if diagnostics.knownSecrets.RedactText(code) != code {
		return &jsonRPCError{Message: message, codeRedacted: true}
	}
	return &jsonRPCError{Code: e.Code, Message: message}
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
	Method          string
	Params          json.RawMessage
	advisoryDropped bool
	address         codexNotificationAddress
	terminalStatus  string
}

type codexObservedTerminal struct {
	status             string
	beforeCancellation bool
}

// CodexRPCClient is a generic JSON-RPC 2.0 client over a pair of stdio-style
// streams. One client corresponds to one app-server process
// (§13.1: "one app-server process per Hufu attempt").
type CodexRPCClient struct {
	diagnosticEnv []string
	knownSecrets  *utils.SecretRedactionScope
	stdin         io.WriteCloser
	writing       chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan jsonRPCResponse

	closed    bool
	closeErr  error
	closeOnce sync.Once
	closeCh   chan struct{}

	notifications           chan CodexNotification
	queuedNotificationBytes atomic.Int64
	advisoryQueueBytes      int64
	notificationTarget      atomic.Pointer[codexNotificationTarget]
	terminalMu              sync.Mutex
	observedTerminal        *codexObservedTerminal
	turnAcknowledged        chan struct{}
	queueUnbound            bool
	stderr                  *codexStderrCapture
	finishProcess           func() error
}

var errCodexPrivateProviderIdentity = errors.New("provider identity contains a credential")

func (c *CodexRPCClient) providerIdentitySafe(id string) bool {
	return id == "" || c.knownSecrets.RedactText(id) == id
}

type jsonRPCResponse struct {
	Result json.RawMessage
	Error  *jsonRPCError

	advisoryDropped  bool
	diagnosticPrefix []CodexNotification
}

// NewCodexRPCClient wraps an already-started process's stdin/stdout.
// maxFrameBytes <= 0 uses defaultCodexMaxFrameBytes.
func NewCodexRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrameBytes int) *CodexRPCClient {
	return newCodexRPCClient(stdin, stdout, maxFrameBytes, codexMaxAdvisoryQueueBytes, false)
}

func newCodexRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrameBytes int, advisoryQueueBytes int64, queueUnbound bool) *CodexRPCClient {
	if maxFrameBytes <= 0 {
		maxFrameBytes = defaultCodexMaxFrameBytes
	}
	c := &CodexRPCClient{
		stdin:              stdin,
		writing:            make(chan struct{}, 1),
		pending:            make(map[int64]chan jsonRPCResponse),
		closeCh:            make(chan struct{}),
		notifications:      make(chan CodexNotification, 1024),
		advisoryQueueBytes: advisoryQueueBytes,
		queueUnbound:       queueUnbound,
		turnAcknowledged:   make(chan struct{}, 1),
	}
	go c.readLoop(stdout, maxFrameBytes)
	return c
}

func (c *CodexRPCClient) acknowledgedTurnID() string {
	if target := c.notificationTarget.Load(); target != nil {
		return target.turnID
	}
	return ""
}

func (c *CodexRPCClient) awaitAcknowledgedTurn(ctx context.Context) string {
	if id := c.acknowledgedTurnID(); id != "" {
		return id
	}
	select {
	case <-c.turnAcknowledged:
	case <-c.Done():
	case <-ctx.Done():
	}
	return c.acknowledgedTurnID()
}

func (c *CodexRPCClient) completedBeforeCancellation() bool {
	terminal := c.observedTurnTerminal()
	return terminal != nil && terminal.status == "completed" && terminal.beforeCancellation
}

func (c *CodexRPCClient) observedTurnTerminal() *codexObservedTerminal {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	return c.observedTerminal
}

// A canceled waiter and the reader deciding whether a terminal preceded
// cancellation use the same lock. Before an ACK, a terminal is provisional.
func (c *CodexRPCClient) publishTurnTerminal(ctx context.Context, status string) {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if c.observedTerminal == nil {
		c.observedTerminal = &codexObservedTerminal{status: status, beforeCancellation: ctx.Err() == nil}
	}
}

// Done is closed once the client can no longer serve requests (process
// exited, transport error, or Close was called).
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
	return c.call(ctx, method, params, result, nil)
}

func (c *CodexRPCClient) call(ctx context.Context, method string, params, result any, redactError func(jsonRPCResponse) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	var target *codexNotificationTarget
	if turn, ok := params.(codexTurnStartParams); method == codexMethodTurnStart && ok {
		target = &codexNotificationTarget{requestID: id, threadID: turn.ThreadID, ctx: ctx}
	}
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

	if writeErr := c.writeRequest(ctx, line, target); writeErr != nil {
		c.removePending(id)
		return fmt.Errorf("write %s request: %w", method, writeErr)
	}

	resp, err := c.waitResponse(ctx, id, ch, target != nil)
	if err != nil {
		if err == ctx.Err() {
			return err
		}
		return fmt.Errorf("codex %s: %w", method, err)
	}
	if resp.Error != nil {
		if redactError != nil {
			return fmt.Errorf("codex %s: %w", method, redactError(resp))
		}
		diagnostics := c.newDiagnostics(nil)
		if method == codexMethodThreadStart || method == codexMethodThreadResume {
			c.finishDiagnosticProcess(diagnostics)
		}
		return fmt.Errorf("codex %s: %w", method, resp.Error.redacted(diagnostics))
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

func (c *CodexRPCClient) writeRequest(ctx context.Context, line []byte, target *codexNotificationTarget) error {
	select {
	case c.writing <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Done():
		return c.Err()
	}
	if err := ctx.Err(); err != nil {
		<-c.writing
		return err
	}
	const (
		writeStarted  = 1
		writeCanceled = 2
	)
	var state atomic.Uint32
	written := make(chan error, 1)
	go func() {
		defer func() { <-c.writing }()
		if err := ctx.Err(); err != nil {
			written <- err
			return
		}
		if !state.CompareAndSwap(0, writeStarted) {
			written <- ctx.Err()
			return
		}
		if target != nil {
			c.notificationTarget.Store(target)
		}
		n, err := c.stdin.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
		written <- err
		if err != nil {
			c.closeWithError(fmt.Errorf("codex app-server request write: %w", err))
		}
	}()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		if state.CompareAndSwap(0, writeCanceled) {
			return ctx.Err()
		}
		if target != nil {
			// The peer may already have the request. Leave its ACK pending
			// while the attempt owner waits through interrupt grace.
			return nil
		}
		// A partial request makes further use of the wire unsafe.
		_ = c.Close()
		return ctx.Err()
	}
}

func (c *CodexRPCClient) waitResponse(ctx context.Context, id int64, ch <-chan jsonRPCResponse, retainTurnStart bool) (jsonRPCResponse, error) {
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		if retainTurnStart && c.completedBeforeCancellation() {
			// The reader already authenticated a successful terminal. It
			// also owns delivery of the corresponding ACK.
			return c.waitResponse(context.WithoutCancel(ctx), id, ch, true)
		}
		if !retainTurnStart {
			c.removePending(id)
		}
		// Keep a sent start eligible for its wire ACK until the owning
		// attempt closes the transport at the end of interrupt grace.
		return jsonRPCResponse{}, ctx.Err()
	case <-c.closeCh:
		// Every reply already published by the reader precedes the frame
		// (or EOF) that stopped it.
		select {
		case resp := <-ch:
			return resp, nil
		default:
			return jsonRPCResponse{}, c.Err()
		}
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

func (n CodexNotification) queuedBytes() int64 {
	return int64(len(n.Method) + cap(n.Params))
}

func (c *CodexRPCClient) dispatchNotification(notif CodexNotification) bool {
	size := notif.queuedBytes()
	c.queuedNotificationBytes.Add(size)
	select {
	case c.notifications <- notif:
		return true
	default:
		c.queuedNotificationBytes.Add(-size)
		return false
	}
}

func (c *CodexRPCClient) releaseNotification(notif CodexNotification) {
	c.queuedNotificationBytes.Add(-notif.queuedBytes())
}

// A single writer preserves wire order. A slot and one maximum-size frame
// are reserved for the terminal notification. Never block RPC replies.
func (c *CodexRPCClient) queueNotification(notif CodexNotification, maxFrameBytes int, dropped *bool, target *codexNotificationTarget) bool {
	kind := notif.kind()
	terminal := kind == codexTerminal
	bytesAfterEnqueue := c.queuedNotificationBytes.Load() + notif.queuedBytes()
	if !terminal && (len(c.notifications) >= cap(c.notifications)-1 || bytesAfterEnqueue > c.advisoryQueueBytes) {
		*dropped = *dropped || kind == codexDiagnostic
		return true
	}
	if bytesAfterEnqueue > c.advisoryQueueBytes+2*int64(maxFrameBytes) {
		return false
	}
	notif.advisoryDropped = terminal && *dropped
	if terminal && len(c.notifications) == cap(c.notifications) {
		return false
	}
	if terminal && target != nil {
		c.publishTurnTerminal(target.ctx, notif.terminalStatus)
	}
	return c.dispatchNotification(notif)
}

func (c *CodexRPCClient) readLoop(stdout io.Reader, maxFrameBytes int) {
	defer func() { _ = c.stdin.Close() }()
	scanner := bufio.NewScanner(stdout)
	initial := 64 * 1024
	if initial > maxFrameBytes {
		initial = maxFrameBytes
	}
	scanner.Buffer(make([]byte, 0, initial), maxFrameBytes)
	advisoryDropped := false
	var beforeAck codexPendingNotifications
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope jsonRPCEnvelope
		if err := jsonv2.Unmarshal(line, &envelope); err != nil {
			c.closeWithError(errors.New("codex app-server sent a malformed JSON-RPC frame"))
			return
		}
		switch {
		case len(envelope.ID) > 0 && envelope.Method == "":
			var id int64
			if err := json.Unmarshal(envelope.ID, &id); err != nil || id <= 0 {
				c.closeWithError(errors.New("codex app-server sent a response with an invalid id"))
				return
			}
			reply := jsonRPCResponse{Result: envelope.Result, Error: envelope.Error}
			if target := c.notificationTarget.Load(); target != nil && target.requestID == id {
				if !c.routePendingNotifications(*target, &reply, &beforeAck, maxFrameBytes, &advisoryDropped) {
					c.closeWithError(errors.New("codex app-server sent too many pending terminal notifications"))
					return
				}
				if c.acknowledgedTurnID() != "" {
					select {
					case c.turnAcknowledged <- struct{}{}:
					default:
					}
				}
			}
			if reply.Error != nil {
				reply.advisoryDropped = reply.advisoryDropped || advisoryDropped
			}
			if !c.deliverResponse(id, reply) {
				c.closeWithError(errors.New("codex app-server sent a response for an unknown request id"))
				return
			}
		case envelope.Method != "" && len(envelope.ID) == 0:
			if !c.routeNotification(CodexNotification{Method: envelope.Method, Params: envelope.Params}, &beforeAck, maxFrameBytes, &advisoryDropped) {
				c.closeWithError(fmt.Errorf("codex app-server sent too many pending terminal notifications"))
				return
			}
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
	c.closeWithError(fmt.Errorf("codex app-server process closed its output stream: %w", io.EOF))
}
