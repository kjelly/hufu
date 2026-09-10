package team

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Phase 4 tests (spec.md §36 PR-08): the generic JSON-RPC client, driven
// against a real fake app-server subprocess (codex_fake_server_test.go),
// never Codex itself.

// TestCodexRPCInitialize proves a basic request/response round trip: the
// client's Call sends a request and correctly decodes the matching response.
func TestCodexRPCInitialize(t *testing.T) {
	server := startFakeCodexServer(t, []fakeCodexStep{
		{Result: rawJSON(t, map[string]any{"userAgent": "fake/1.0", "codexHome": "/fake/home"})},
	})
	defer server.Client.Close()

	result, err := codexInitialize(context.Background(), server.Client)
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

// TestCodexRPCOversizedFrameFails proves a single JSON-RPC line larger than
// the configured frame bound terminates the client as a protocol violation
// (§17.2) rather than hanging or silently truncating.
func TestCodexRPCOversizedFrameFails(t *testing.T) {
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
