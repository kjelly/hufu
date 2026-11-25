package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodexDelayedAcknowledgementCanBeInterruptedAfterCancel(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	gate := filepath.Join(t.TempDir(), "turn-start-reply")
	server := startFakeCodexServer(t, []fakeCodexStep{
		{ReplyGate: gate, Result: json.RawMessage(`{"turn":{"id":"late-turn"}}`)}, {},
	})
	defer server.Client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := codexRunTurn(ctx, server.Client, "late-thread", "work", codexTurnOptions{})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(gate + ".waiting")
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatalf("script did not receive turn/start: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled response waiter returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response waiter did not return before the ACK")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	interruptCodexTurn(server.Client, "late-thread", 2*time.Second)
	if id := server.Client.acknowledgedTurnID(); id != "late-turn" {
		t.Fatalf("late acknowledged turn = %q, error %v", id, server.Client.Err())
	}
	if calls := server.CallLog(t); !slices.Equal(calls, []string{codexMethodTurnStart, codexMethodTurnInterrupt}) {
		t.Fatalf("wire calls = %v", calls)
	}
}

func TestCodexAuthenticatedTerminalAndCancellationPrecedence(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, status := range []string{"completed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			step := fakeCodexTurnCompletedStep(t, "terminal-turn", validProposalJSON(""))
			if status == "interrupted" {
				step.Notifications[0].Params = json.RawMessage(`{"turn":{"id":"terminal-turn","status":"interrupted"}}`)
			}
			step.AfterResponse, step.Notifications = step.Notifications, nil
			server := startFakeCodexServer(t, []fakeCodexStep{step, {}})
			defer server.Client.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, turnErr := codexRunTurn(ctx, server.Client, "terminal-thread", "work", codexTurnOptions{
				OnTurnStarted: func(string) error {
					// The reply to this probe follows the terminal on the
					// same wire, so cancellation follows its observation.
					if err := server.Client.Call(ctx, "probe", nil, nil); err != nil {
						t.Fatal(err)
					}
					cancel()
					return nil
				},
			})
			terminal := server.Client.observedTurnTerminal()
			if terminal == nil || terminal.status != status || !terminal.beforeCancellation {
				t.Fatalf("unexpected authenticated terminal %+v", terminal)
			}
			failure := codexAttemptContextFailure(ctx, 0, server.Client.completedBeforeCancellation())
			if status == "completed" {
				if failure != nil || turnErr != nil || result.Proposal == nil {
					t.Fatalf("completed turn was lost: %v, %v", failure, turnErr)
				}
			} else if classified, ok := errors.AsType[*CodexProviderError](failure); !ok || classified.Class != CodexFailureCancelled {
				t.Fatalf("interrupted turn lost cancellation disposition: %v", failure)
			}
		})
	}
}

// Phase 4 tests (spec.md §36 PR-08): the generic JSON-RPC client, driven
// against a real fake app-server subprocess (codex_fake_server_test.go),
// never Codex itself.

// TestCodexRPCInitialize proves a basic request/response round trip: the
// client's Call sends a request and correctly decodes the matching response.
func TestCodexRPCInitialize(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	server := startFakeCodexServer(t, []fakeCodexStep{
		{Result: rawJSON(t, map[string]any{"userAgent": "fake/1.0", "codexHome": "/fake/home"})},
	})
	defer server.Client.Close()

	result, err := codexInitialize(context.Background(), server.Client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.UserAgent != "fake/1.0" || result.CodexHome != "/fake/home" {
		t.Fatalf("initialize result = %#v, want the fake server's values", result)
	}
}

// TestCodexRPCUnknownNotificationIgnored proves an unrecognized notification
// method never crashes the client and never blocks a subsequent call.
func TestCodexRPCUnknownNotificationIgnored(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	server := startFakeCodexServer(t, []fakeCodexStep{
		{
			Notifications: []fakeCodexNotification{{Method: "totally/unrecognized", Params: rawJSON(t, map[string]any{"anything": true})}},
			Result:        rawJSON(t, map[string]any{"ok": true}),
		},
		{Result: rawJSON(t, map[string]any{"ok": true})},
	})
	defer server.Client.Close()

	if err := server.Client.Call(context.Background(), "probe", nil, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// The unknown notification must not have wedged anything; a second,
	// unrelated call still succeeds normally.
	if err := server.Client.Call(context.Background(), "probe", nil, nil); err != nil {
		t.Fatalf("second call after unknown notification: %v", err)
	}
}

func TestCodexRPCPreservesTerminalAfterAdvisoryQueueFills(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	const advisoryCount = 1100
	notifications := make([]fakeCodexNotification, 0, advisoryCount+1)
	for index := range advisoryCount {
		notifications = append(notifications, fakeCodexNotification{Method: "item/completed", Params: rawJSON(t, index)})
	}
	notifications = append(notifications, fakeCodexNotification{Method: codexNotificationTurnCompleted, Params: rawJSON(t, nil)})
	server := startFakeCodexServer(t, []fakeCodexStep{{Notifications: notifications}})
	defer server.Client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := server.Client.Call(ctx, "probe", nil, nil); err != nil {
		t.Fatalf("advisory backlog blocked the RPC reply: %v", err)
	}
	queue := server.Client.notifications
	if len(queue) != cap(queue) {
		t.Fatal("RPC reply arrived before the pending notification queue was filled")
	}
	for index := range cap(queue) - 1 {
		notif := <-queue
		server.Client.releaseNotification(notif)
		if notif.Method != "item/completed" || string(notif.Params) != strconv.Itoa(index) {
			t.Fatalf("advisory notification %d went out of order", index)
		}
	}
	select {
	case terminal := <-queue:
		server.Client.releaseNotification(terminal)
		if terminal.Method != codexNotificationTurnCompleted || !terminal.advisoryDropped {
			t.Fatal("terminal notification must survive and disclose dropped telemetry")
		}
	case <-ctx.Done():
		t.Fatal("terminal notification was lost")
	}
}

func TestCodexRPCAdvisoryByteLimitPreservesTerminalAndSuppressesDetail(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	const budget = 128
	advisory := func(text string) fakeCodexNotification {
		return fakeCodexNotification{Method: "item/completed", Params: rawJSON(t, map[string]any{
			"item": map[string]string{"type": "agentMessage", "text": text},
		})}
	}
	terminal := fakeCodexTurnCompletedStep(t, "turn", validProposalJSON("")).Notifications
	server := startFakeCodexServerWithReadLimits(t, []fakeCodexStep{
		{Notifications: []fakeCodexNotification{advisory("first"), advisory(strings.Repeat("x", 2*budget))}},
		{Notifications: append([]fakeCodexNotification{advisory("unverified detail")}, terminal...)},
	}, 0, budget)
	defer server.Client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := server.Client.Call(ctx, "probe", nil, nil); err != nil {
		t.Fatal(err)
	}
	queue := server.Client.notifications
	if len(queue) != 1 || server.Client.queuedNotificationBytes.Load() > budget {
		t.Fatal("pending advisory data exceeded its byte limit")
	}
	server.Client.releaseNotification(<-queue)
	if server.Client.queuedNotificationBytes.Load() != 0 {
		t.Fatal("consuming a notification did not release its byte allowance")
	}
	if err := server.Client.Call(ctx, "probe", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 {
		t.Fatal("released byte allowance did not admit one advisory and the terminal frame")
	}
	var activity []string
	turn, err := waitForCodexTurnCompleted(ctx, server.Client, "thread", "turn", func(event codexTurnActivity) {
		activity = append(activity, event.Message)
	}, true, newCodexTurnDiagnostics(nil))
	if err != nil || len(turn.ID) == 0 || !codexOptionalIDMatches(turn.ID, "turn") || !turn.diagnosticsUnsafe || len(activity) != 0 {
		t.Fatalf("terminal delivery did not suppress incomplete diagnostics: unsafe=%t activity=%d error=%v", turn.diagnosticsUnsafe, len(activity), err)
	}
}

func TestCodexTurnRoutesStaleSameThreadFramesAtAcknowledgment(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	for _, tc := range []struct {
		name          string
		afterAck      bool
		terminalFirst bool
		activeLoss    bool
		unsafe        bool
		terminalLost  bool
	}{
		{name: "stale before acknowledgment"},
		{name: "stale after acknowledgment", afterAck: true},
		{name: "early terminal before stale burst", terminalFirst: true},
		{name: "active diagnostic was lost", activeLoss: true, unsafe: true},
		{name: "active loss was after terminal", terminalFirst: true, activeLoss: true},
		{name: "active terminal exceeded candidate budget", terminalLost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const frameLimit = 4096
			const advisoryLimit = 512
			oversizedItem := func(id string) fakeCodexNotification {
				return fakeCodexNotification{Method: "item/completed", Params: rawJSON(t, map[string]any{
					"threadId": "thread", "turnId": id, "item": map[string]string{"type": "agentMessage", "text": strings.Repeat("s", advisoryLimit)},
				})}
			}
			step := fakeCodexTurnCompletedStep(t, "active-turn", validProposalJSON(""))
			var burst []fakeCodexNotification
			for old := range 5711 {
				burst = append(burst, oversizedItem(fmt.Sprintf("old-item-%d", old)))
			}
			for old := range 1025 {
				padding := ""
				if old < 4 {
					padding = strings.Repeat("s", frameLimit-400)
				}
				burst = append(burst, fakeCodexNotification{Method: codexNotificationTurnCompleted, Params: rawJSON(t, map[string]any{
					"threadId": "thread", "turn": map[string]string{"id": fmt.Sprintf("old-turn-%d", old), "status": "completed"},
					"padding": padding,
				})})
			}
			if tc.activeLoss {
				burst = append(burst, oversizedItem("active-turn"))
			}
			if tc.terminalFirst {
				step.Notifications = append(step.Notifications, burst...)
			} else {
				step.Notifications = append(burst, step.Notifications...)
			}
			if tc.terminalLost {
				step.Notifications = append(step.Notifications, fakeCodexNotification{
					Method: codexNotificationTurnCompleted,
					Params: rawJSON(t, map[string]any{"threadId": "thread", "turn": map[string]string{"id": "another-old-turn", "status": "completed"}}),
				})
			}
			if tc.afterAck {
				step.AfterResponse, step.Notifications = step.Notifications, nil
			}
			server := startFakeCodexServerWithReadLimits(t, []fakeCodexStep{step}, frameLimit, advisoryLimit)
			defer server.Client.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var activity []string
			result, err := codexRunTurn(ctx, server.Client, "thread", "work", codexTurnOptions{
				DetailedOutput: true,
				OnActivity: func(event codexTurnActivity) {
					activity = append(activity, event.Method)
				},
			})
			if tc.terminalLost {
				failure, ok := errors.AsType[*CodexProviderError](err)
				if !ok || failure.Class != CodexFailureProtocolError || result.TurnID != "active-turn" || result.Proposal != nil {
					t.Fatalf("exhausted terminal budget did not preserve the ACK and fail as protocol overflow: %+v / %v", result, err)
				}
				return
			}
			if err != nil || result.TurnID != "active-turn" || result.Proposal == nil || result.diagnosticsUnsafe != tc.unsafe || slices.Contains(activity, "item/completed") {
				t.Fatalf("notification routing = error %v, unsafe %t, activity %v; want unsafe %t", err, result.diagnosticsUnsafe, activity, tc.unsafe)
			}
		})
	}
}

func TestCodexDrainedDiagnosticsRemainBoundedUntilTerminal(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	for _, valid := range []bool{false, true} {
		t.Run(strconv.FormatBool(valid), func(t *testing.T) {
			const batch = 64
			const probes = codexMaxDeferredActivityCount/batch + 2
			delta := fakeCodexNotification{Method: codexCommandOutputDelta}
			if valid {
				delta.Params = rawJSON(t, map[string]string{"itemId": "command", "delta": "x"})
			}
			useful := fakeCodexNotification{Method: "item/completed", Params: rawJSON(t, map[string]any{
				"item": map[string]string{"type": "agentMessage", "text": "retained diagnostic"},
			})}
			terminal := fakeCodexTurnCompletedStep(t, "turn", validProposalJSON(""))
			steps := []fakeCodexStep{{Result: terminal.Result, Notifications: []fakeCodexNotification{useful}}}
			notifications := make([]fakeCodexNotification, batch, batch+1)
			for i := range notifications {
				notifications[i] = delta
			}
			notifications = append(notifications, fakeCodexNotification{Method: "turn/plan/updated"})
			for range probes {
				steps = append(steps, fakeCodexStep{Notifications: notifications})
			}
			steps = append(steps, fakeCodexStep{Notifications: terminal.Notifications, CloseAfterResponse: true})
			server := startFakeCodexServer(t, steps)
			defer server.Client.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started, drained := make(chan struct{}), make(chan struct{}, 1)
			done := make(chan struct{})
			var result CodexTurnResult
			var turnErr error
			var detailed []string
			go func() {
				defer close(done)
				result, turnErr = codexRunTurn(ctx, server.Client, "thread", "probe", codexTurnOptions{
					DetailedOutput: true,
					OnTurnStarted: func(string) error {
						close(started)
						return nil
					},
					OnActivity: func(activity codexTurnActivity) {
						switch activity.Method {
						case "turn/plan/updated":
							drained <- struct{}{}
						case "item/completed", codexCommandOutputDelta:
							detailed = append(detailed, activity.Message)
						}
					},
				})
			}()
			await := func(signal <-chan struct{}) {
				t.Helper()
				select {
				case <-signal:
				case <-ctx.Done():
					t.Fatalf("timed out waiting for fake app-server: %v", ctx.Err())
				}
			}
			await(started)
			for range probes {
				if err := server.Client.Call(ctx, "probe", nil, nil); err != nil {
					t.Fatal(err)
				}
				await(drained)
			}
			if err := server.Client.Call(ctx, "finish", nil, nil); err != nil {
				t.Fatal(err)
			}
			await(done)
			if turnErr != nil || result.Proposal == nil || result.TurnID != "turn" || result.diagnosticsUnsafe != valid {
				t.Fatalf("turn state: id=%q proposal=%t diagnostics unsafe=%t error=%v", result.TurnID, result.Proposal != nil, result.diagnosticsUnsafe, turnErr)
			}
			if valid && len(detailed) != 0 || !valid && (len(detailed) != 1 || !strings.Contains(detailed[0], "retained diagnostic")) {
				t.Fatalf("unexpected details after draining diagnostics: %q", detailed)
			}
		})
	}
}

func TestCodexDiagnosticCredentialOverflowSuppressesProviderDetail(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	const first = "opaque-diagnostic-credential-0000"
	distractors := make([]map[string]string, 300)
	for i := range distractors {
		distractors[i] = map[string]string{"api_key": fmt.Sprintf("opaque-diagnostic-credential-%04d", i)}
	}
	proposal := strings.Replace(validProposalJSON(""), "did the work", "provider detail "+first, 1)
	step := fakeCodexTurnCompletedStep(t, "turn", proposal)
	step.CloseAfterResponse = true
	step.Notifications = append([]fakeCodexNotification{
		{Method: "item/completed", Params: rawJSON(t, map[string]any{"item": map[string]string{
			"type": "agentMessage", "text": "earlier diagnostic " + first,
		}})},
		{Method: "error", Params: rawJSON(t, map[string]any{"message": "reported advisory", "metadata": distractors})},
	}, step.Notifications...)
	server := startFakeCodexServer(t, []fakeCodexStep{step})
	defer server.Client.Close()
	var output []string
	result, err := codexRunTurn(t.Context(), server.Client, "thread", "probe", codexTurnOptions{
		DetailedOutput: true,
		OnActivity: func(activity codexTurnActivity) {
			output = append(output, activity.Message)
		},
	})
	if err != nil || result.Proposal == nil || !strings.Contains(result.Proposal.Summary, first) {
		t.Fatalf("diagnostic overflow changed canonical proposal: %v", err)
	}
	if !result.diagnosticsUnsafe || result.diagnosticOutput() != "[invalid provider JSON]" {
		t.Fatal("diagnostic overflow retained provider output")
	}
	if joined := strings.Join(output, "\n"); !strings.Contains(joined, "Codex reported an app-server error") ||
		strings.Contains(joined, first) || strings.Contains(joined, "earlier diagnostic") || strings.Contains(joined, "Codex final response") {
		t.Fatal("only generic status may survive incomplete credential discovery")
	}
}

func TestCodexRPCDeliversQueuedTurnAndReplyAtEOF(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	for _, name := range []string{"complete", "absent terminal IDs", "mismatch followed by match",
		"wrong terminal thread", "wrong terminal turn", "blank terminal ID", "wrong ack thread", "missing ack ID",
		"blank ack ID", "null ack thread", "null ack turn", "null terminal thread", "null terminal turn",
		"invalid terminal turn", "duplicate ack thread", "duplicate ack turn", "duplicate terminal thread", "duplicate terminal turn",
		"duplicate reply id", "duplicate reply result", "duplicate notification params", "duplicate after completion",
		"missing terminal", "missing reply", "malformed", "malformed after completion", "malformed after ack"} {
		t.Run(name, func(t *testing.T) {
			step := fakeCodexTurnCompletedStep(t, "turn", validProposalJSON(""))
			step.CloseAfterResponse = true
			notification := func(threadID, turnID any) fakeCodexNotification {
				turn := map[string]any{"status": "completed",
					"items": []map[string]string{{"type": "agentMessage", "phase": "final_answer", "text": validProposalJSON("")}}}
				if turnID != nil {
					turn["id"] = turnID
				}
				params := map[string]any{"turn": turn}
				if threadID != nil {
					params["threadId"] = threadID
				}
				return fakeCodexNotification{Method: codexNotificationTurnCompleted, Params: rawJSON(t, params)}
			}
			switch name {
			case "complete":
				step.Notifications = []fakeCodexNotification{notification("thread", "turn")}
			case "absent terminal IDs":
				step.Notifications = []fakeCodexNotification{notification(nil, nil)}
			case "wrong terminal thread":
				step.Notifications = []fakeCodexNotification{notification("wrong", "turn")}
			case "wrong terminal turn":
				step.Notifications = []fakeCodexNotification{notification("thread", "wrong")}
			case "blank terminal ID":
				step.Notifications = []fakeCodexNotification{notification("", "turn")}
			case "mismatch followed by match":
				step.Notifications = []fakeCodexNotification{notification("wrong", "turn"), notification("thread", "turn")}
			case "wrong ack thread":
				step.Result = rawJSON(t, map[string]any{"threadId": "wrong", "turn": map[string]string{"id": "turn"}})
			case "missing ack ID":
				step.Result = rawJSON(t, map[string]any{"turn": map[string]string{}})
			case "blank ack ID":
				step.Result = rawJSON(t, map[string]any{"turn": map[string]string{"id": " \t "}})
			case "null ack thread":
				step.Result = json.RawMessage(`{"threadId":null,"turn":{"id":"turn"}}`)
			case "null ack turn":
				step.Result = json.RawMessage(`{"turn":{"id":null}}`)
			case "null terminal thread":
				step.Notifications = []fakeCodexNotification{notification(json.RawMessage(`null`), "turn")}
			case "null terminal turn":
				step.Notifications = []fakeCodexNotification{notification("thread", json.RawMessage(`null`))}
			case "invalid terminal turn":
				step.Notifications = []fakeCodexNotification{notification("thread", 42)}
			case "duplicate ack thread":
				step.Result = json.RawMessage(`{"threadId":null,"threadId":"thread","turn":{"id":"turn"}}`)
			case "duplicate ack turn":
				step.Result = json.RawMessage(`{"turn":{"id":"wrong","id":"turn"}}`)
			case "duplicate terminal thread":
				frame := notification("thread", "turn")
				frame.Params = json.RawMessage(strings.Replace(string(frame.Params), `"threadId":"thread"`, `"threadId":null,"threadId":"thread"`, 1))
				step.Notifications = []fakeCodexNotification{frame}
			case "duplicate terminal turn":
				frame := notification("thread", "turn")
				frame.Params = json.RawMessage(strings.Replace(string(frame.Params), `"id":"turn"`, `"id":"wrong","id":"turn"`, 1))
				step.Notifications = []fakeCodexNotification{frame}
			case "duplicate reply id":
				step.RawLine = `{"jsonrpc":"2.0","id":200,"id":1,"result":{"turn":{"id":"turn"}}}`
			case "duplicate reply result":
				step.RawLine = `{"jsonrpc":"2.0","id":1,"result":{"turn":{"id":"wrong"}},"result":{"turn":{"id":"turn"}}}`
			case "duplicate notification params", "duplicate after completion":
				if name == "duplicate notification params" {
					step.Notifications = nil
				}
				frame := notification("thread", "turn")
				step.RawLine = `{"jsonrpc":"2.0","id":1,"result":{"turn":{"id":"turn"}}}` + "\n" +
					`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"wrong"},"params":` + string(frame.Params) + `}`
			case "malformed after completion", "malformed after ack":
				step.RawLine = `{"jsonrpc":"2.0","id":1,"result":{"turn":{"id":"turn"}}}` + "\n{"
				if name == "malformed after ack" {
					step.Notifications = nil
				}
			case "missing terminal":
				step.Notifications = nil
			case "missing reply":
				step.CloseAfter = true
			case "malformed":
				step.Notifications = nil
				step.RawLine = "{"
			}
			server := startFakeCodexServer(t, []fakeCodexStep{step})
			defer server.Client.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var started bool
			result, err := codexRunTurn(ctx, server.Client, "thread", "probe", codexTurnOptions{
				OnTurnStarted: func(string) error {
					started = true
					select {
					case <-server.Client.Done():
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			switch name {
			case "complete", "absent terminal IDs", "mismatch followed by match", "malformed after completion", "duplicate after completion":
				if err != nil || !started || result.TurnID != "turn" || result.Proposal == nil {
					t.Fatalf("EOF lost a completed turn or reply: id=%q err=%v", result.TurnID, err)
				}
			case "wrong ack thread", "missing ack ID", "blank ack ID", "null ack thread", "null ack turn", "duplicate ack thread", "duplicate ack turn":
				if err == nil || started || result.Proposal != nil || !strings.Contains(err.Error(), "codex turn/start") {
					t.Fatalf("accepted an uncorrelated acknowledgement: id=%q started=%t error=%v", result.TurnID, started, err)
				}
			case "malformed", "malformed after ack", "duplicate reply id", "duplicate reply result", "duplicate notification params",
				"duplicate terminal thread", "duplicate terminal turn":
				wantStarted := name == "malformed after ack" || name == "duplicate notification params"
				if err == nil || errors.Is(err, io.EOF) || started != wantStarted || !strings.Contains(err.Error(), "malformed JSON-RPC frame") {
					t.Fatalf("expected framing error, got %v", err)
				}
			default:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("expected incomplete-stream error, got %v", err)
				}
			}
		})
	}
}

// TestCodexRPCInvalidResponseIDsFailClosed proves that a response cannot
// strand a pending request by carrying a non-integer or unknown ID. JSON-RPC
// response IDs are part of the transport correlation invariant: malformed
// and unmatched IDs must terminate the client rather than being ignored.
func TestCodexRPCInvalidResponseIDsFailClosed(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "string", id: `"1"`, want: "invalid id"},
		{name: "fractional", id: "1.5", want: "invalid id"},
		{name: "null", id: "null", want: "invalid id"},
		{name: "unknown integer", id: "999", want: "unknown request id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := startFakeCodexServer(t, []fakeCodexStep{
				{RawLine: `{"jsonrpc":"2.0","id":` + tc.id + `,"result":{}}`},
			})
			defer server.Client.Close()

			err := server.Client.Call(context.Background(), "probe", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Call error = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestCodexRPCOversizedFrameFails proves a single JSON-RPC line larger than
// the configured frame bound terminates the client as a protocol violation
// (§17.2) rather than hanging or silently truncating.
func TestCodexRPCOversizedFrameFails(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	oversized := strings.Repeat("x", 4096)
	server := startFakeCodexServerWithFrameBound(t, []fakeCodexStep{
		{RawLine: `{"jsonrpc":"2.0","id":1,"result":{"padding":"` + oversized + `"}}`},
	}, 256)
	defer server.Client.Close()

	if err := server.Client.Call(context.Background(), "probe", nil, nil); err == nil {
		t.Fatal("expected an oversized frame to fail the call")
	}
}

// TestCodexRPCDefaultFrameAcceptsLargeCodexEvent proves the production
// default is still finite but no longer rejects a response merely because it
// crosses Scanner's historical 1 MiB default. The explicit small-bound test
// above preserves fail-closed rejection for genuinely oversized frames.
func TestCodexRPCDefaultFrameAcceptsLargeCodexEvent(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	largeEvent := strings.Repeat("x", (1<<20)+4096)
	server := startFakeCodexServer(t, []fakeCodexStep{
		{RawLine: `{"jsonrpc":"2.0","id":1,"result":{"padding":"` + largeEvent + `"}}`},
	})
	defer server.Client.Close()

	if err := server.Client.Call(context.Background(), "probe", nil, nil); err != nil {
		t.Fatalf("large event above the legacy 1 MiB Scanner limit was rejected: %v", err)
	}
}

// TestCodexRPCProcessCrashFailsInflightRequests proves that when the
// app-server process exits without responding, every in-flight Call fails
// promptly instead of hanging forever.
func TestCodexRPCProcessCrashFailsInflightRequests(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	server := startFakeCodexServer(t, []fakeCodexStep{
		{CloseAfter: true},
	})
	defer server.Client.Close()

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			errs <- server.Client.Call(context.Background(), "probe", nil, nil)
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("expected the in-flight call to fail after the process crashed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight call did not fail within 5s of the process crashing")
		}
	}
}

// TestCodexRPCCancellation proves ctx cancellation on Call returns promptly
// with a context error instead of waiting for a response that never comes.
func TestCodexRPCCancellation(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	server := startFakeCodexServer(t, []fakeCodexStep{
		{NoResponse: true},
	})
	defer server.Client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Client.Call(ctx, "probe", nil, nil) }()

	// Give the request time to actually be sent before cancelling, so this
	// exercises "cancel while in flight" rather than "cancel before send".
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("Call error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return within 2s of ctx cancellation")
	}
}

func TestCodexRPCWriteHonorsCancellationWhenPeerDoesNotRead(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	input, stdin := io.Pipe()
	stdout, output := io.Pipe()
	t.Cleanup(func() {
		_ = output.Close()
		_ = input.Close()
	})
	client := NewCodexRPCClient(stdin, stdout, 0)
	defer client.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := client.Call(ctx, "probe", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write returned %v, want context deadline exceeded", err)
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("client retained a potentially partial request after cancellation")
	}
}

func TestCodexTurnStartRPCErrorSurvivesFullDiagnostics(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	const secret = "queue-sensitive-88125"
	item := fakeCodexNotification{
		Method: "item/completed",
		Params: rawJSON(t, map[string]any{"item": map[string]string{"type": "commandExecution", "output": "API_TOKEN=" + secret}}),
	}
	notifications := make([]fakeCodexNotification, 1023, 1025)
	for i := range notifications {
		notifications[i] = item
	}
	for _, id := range []string{"terminal-one", "terminal-two"} {
		notifications = append(notifications, fakeCodexNotification{Method: codexNotificationTurnCompleted,
			Params: rawJSON(t, map[string]any{"turn": map[string]string{"id": id, "status": "failed"}}),
		})
	}
	server := startFakeCodexServer(t, []fakeCodexStep{{
		ErrorMessage: "dispatch failed " + secret, Notifications: notifications, CloseAfterResponse: true,
	}})
	defer server.Client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := codexRunTurn(ctx, server.Client, "thread", "work", codexTurnOptions{})
	rpc, ok := errors.AsType[*jsonRPCError](err)
	if !ok || rpc.Code != -32000 || strings.Contains(err.Error(), secret) || result.TurnID != "" {
		t.Fatalf("start did not retain its typed redacted RPC error: %v", err)
	}
}

func TestCodexTurnStartCanceledBeforeWriteDoesNotReachPeer(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	t.Parallel()
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy writer %t", busy), func(t *testing.T) {
			server := startFakeCodexServer(t, []fakeCodexStep{fakeCodexTurnCompletedStep(t, "fresh-turn", validProposalJSON(""))})
			defer server.Client.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if busy {
				server.Client.writing <- struct{}{}
			} else {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := codexRunTurn(ctx, server.Client, "thread", "canceled", codexTurnOptions{})
				done <- err
			}()
			if busy {
				for until := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
					server.Client.mu.Lock()
					pending := len(server.Client.pending)
					server.Client.mu.Unlock()
					if pending != 0 {
						break
					}
					if time.Now().After(until) {
						t.Fatal("request did not wait for the occupied writer")
					}
				}
				cancel()
				<-server.Client.writing
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled turn error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled turn remained blocked")
			}
			if server.Client.notificationTarget.Load() != nil {
				t.Fatal("canceled request published a turn target")
			}
			ready, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelReady()
			result, err := codexRunTurn(ready, server.Client, "thread", "fresh", codexTurnOptions{})
			if err != nil || result.TurnID != "fresh-turn" || !slices.Equal(server.CallLog(t), []string{codexMethodTurnStart}) {
				t.Fatalf("first real turn did not own the one wire request: %v", err)
			}
		})
	}
}
