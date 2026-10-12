package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/conversation"
)

func newFakeChatAdapter(coord *fakeChatCoordinator) *chatRuntimeAdapter {
	adapter := newChatRuntimeAdapter(&teamContext{teamName: "alpha"}, "alpha")
	var persisted []string
	adapter.newDeps = func(*teamContext, string) chatTurnDeps {
		return fakeChatTurnDeps(coord, &persisted)
	}
	return adapter
}

var _ conversation.RuntimePort = (*chatRuntimeAdapter)(nil)

func TestChatRuntimeAdapterRunTurn(t *testing.T) {
	preserveInvocationID(t)
	tests := []struct {
		name          string
		results       []error
		cancelSecond  bool
		wantCalls     []string
		wantCancelled []bool
	}{
		{name: "run then continue", results: []error{nil, nil}, wantCalls: []string{"run", "continue"}, wantCancelled: []bool{false, false}},
		{name: "cancelled turn", results: []error{nil, context.Canceled}, cancelSecond: true, wantCalls: []string{"run", "continue"}, wantCancelled: []bool{false, true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord := &fakeChatCoordinator{results: append([]error(nil), tt.results...), answer: "ok", publish: true}
			adapter := newFakeChatAdapter(coord)
			for i, turnID := range []string{"turn-a", "turn-b"} {
				ctx, cancel := context.WithCancel(context.Background())
				if i == 1 && tt.cancelSecond {
					coord.cancel = cancel
				}
				result := adapter.RunTurn(ctx, turnID, "hello")
				cancel()
				if result.Cancelled != tt.wantCancelled[i] {
					t.Fatalf("turn %d Cancelled = %v, want %v", i, result.Cancelled, tt.wantCancelled[i])
				}
				if len(result.RunIDs) != 1 || result.RunIDs[0] != "run-1" {
					t.Fatalf("RunIDs = %v, want [run-1]", result.RunIDs)
				}
				if result.Result == nil {
					t.Fatal("expected the fresh RunResult to be passed through")
				}
			}
			if got := coord.invocationIDs; len(got) != 2 || got[0] != "turn-a" || got[1] != "turn-b" {
				t.Fatalf("invocation IDs = %v, want the controller turn IDs", got)
			}
			for i := range tt.wantCalls {
				if coord.calls[i] != tt.wantCalls[i] {
					t.Fatalf("calls = %v, want %v", coord.calls, tt.wantCalls)
				}
			}
		})
	}
}

func TestChatRuntimeAdapterControlCommands(t *testing.T) {
	preserveInvocationID(t)
	coord := &fakeChatCoordinator{results: []error{nil, nil, nil}, answer: "ok"}
	adapter := newFakeChatAdapter(coord)
	if err := adapter.ResetContext(context.Background()); !errors.Is(err, errChatControlUnavailable) {
		t.Fatalf("ResetContext without hook = %v", err)
	}
	if err := adapter.SwitchTeam(context.Background(), "beta"); !errors.Is(err, errChatControlUnavailable) {
		t.Fatalf("SwitchTeam without hook = %v", err)
	}

	adapter.RunTurn(context.Background(), "t1", "hello")
	resets := 0
	adapter.resetHook = func(context.Context, *teamContext) error { resets++; return nil }
	if err := adapter.ResetContext(context.Background()); err != nil {
		t.Fatalf("ResetContext = %v", err)
	}
	adapter.RunTurn(context.Background(), "t2", "again")

	adapter.switchHook = func(_ context.Context, current *teamContext, name string) (*teamContext, error) {
		if current.teamName != "alpha" {
			t.Fatalf("switch saw current team %q", current.teamName)
		}
		return &teamContext{teamName: name}, nil
	}
	if err := adapter.SwitchTeam(context.Background(), "beta"); err != nil {
		t.Fatalf("SwitchTeam = %v", err)
	}
	if _, name := adapter.Current(); name != "beta" {
		t.Fatalf("current team = %q, want beta", name)
	}
	adapter.RunTurn(context.Background(), "t3", "third")
	want := []string{"run", "run", "run"}
	for i := range want {
		if coord.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v (reset and switch restart the sequence)", coord.calls, want)
		}
	}
	if resets != 1 {
		t.Fatalf("reset hook calls = %d, want 1", resets)
	}
}
