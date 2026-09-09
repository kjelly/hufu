package team

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Phase 4 tests (spec.md §36 PR-09): thread/start, thread/resume, and the
// effective-state verification §14.1/§14.2 require, against a real fake
// app-server subprocess.

// fakeCodexSandboxPolicy builds the response-side sandbox policy object the
// real app-server actually returns (a discriminated union), from Hufu's
// plain request-side mode string — see codexSandboxPolicyMode's doc comment
// for why the response shape differs from the request shape.
func fakeCodexSandboxPolicy(mode string) map[string]any {
	switch mode {
	case "read-only":
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite", "networkAccess": false}
	default:
		return map[string]any{"type": "dangerFullAccess"}
	}
}

func fakeCodexThreadStartStep(threadID, cwd, model, sandbox string, t *testing.T) fakeCodexStep {
	return fakeCodexStep{Result: rawJSON(t, map[string]any{
		"thread":  map[string]any{"id": threadID},
		"cwd":     cwd,
		"model":   model,
		"sandbox": fakeCodexSandboxPolicy(sandbox),
	})}
}

// fakeCodexTurnCompletedStep acks turn/start and immediately signals
// turn/completed with a Turn whose items embed finalOutput as the terminal
// agentMessage — mirroring the real protocol's own completion payload.
func fakeCodexTurnCompletedStep(t *testing.T, turnID, finalOutput string) fakeCodexStep {
	turn := map[string]any{
		"id":     turnID,
		"status": "completed",
		"items": []map[string]any{
			{"type": "agentMessage", "phase": "final_answer", "text": finalOutput},
		},
	}
	return fakeCodexStep{
		Result:        rawJSON(t, map[string]any{"turn": map[string]any{"id": turnID}}),
		Notifications: []fakeCodexNotification{{Method: "turn/completed", Params: rawJSON(t, map[string]any{"threadId": "thread-1", "turn": turn})}},
	}
}

// TestCodexNewThreadPersistsBindingBeforeTurn proves §7.4's "session binding
// MUST be persisted before starting a later retry/resume that depends on
// it": the durable-persist callback fires with the new thread's effective
// state before any turn can start, and — the half that actually matters
// operationally — a persist failure blocks turn/start from ever being sent,
// rather than merely being logged after the fact.
func TestCodexNewThreadPersistsBindingBeforeTurn(t *testing.T) {
	const cwd = "/workspace/task-1"
	cfg := CodexThreadConfig{CWD: cwd, Model: "gpt-5-codex", Sandbox: "workspace-write"}

	t.Run("persist success allows the turn to proceed", func(t *testing.T) {
		server := startFakeCodexServer(t, []fakeCodexStep{
			fakeCodexThreadStartStep("thread-1", cwd, "gpt-5-codex", "workspace-write", t),
			fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
		})
		defer server.Client.Close()

		var persisted []CodexEffectiveThreadState
		effective, err := codexStartOrResumeThread(context.Background(), server.Client, "", cfg, func(state CodexEffectiveThreadState) error {
			persisted = append(persisted, state)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(persisted) != 1 || persisted[0].ThreadID != "thread-1" {
			t.Fatalf("persisted bindings = %#v, want exactly one for thread-1", persisted)
		}

		turnResult, err := codexRunTurn(context.Background(), server.Client, effective.ThreadID, "do the work", codexTurnOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if turnResult.Proposal == nil {
			t.Fatal("expected a decoded proposal")
		}
		if got := server.CallLog(t); !slices.Equal(got, []string{"thread/start", "turn/start"}) {
			t.Fatalf("call log = %v, want thread/start, turn/start in that order (no thread/read: the completion notification already carries the final answer)", got)
		}
	})

	t.Run("persist failure blocks the turn from ever starting", func(t *testing.T) {
		server := startFakeCodexServer(t, []fakeCodexStep{
			fakeCodexThreadStartStep("thread-2", cwd, "gpt-5-codex", "workspace-write", t),
			{Result: rawJSON(t, map[string]any{"turn": map[string]any{"id": "turn-2"}})}, // must never be consumed
		})
		defer server.Client.Close()

		_, err := codexStartOrResumeThread(context.Background(), server.Client, "", cfg, func(CodexEffectiveThreadState) error {
			return fmt.Errorf("injected durable persist failure")
		})
		if err == nil {
			t.Fatal("expected a persist failure to fail codexStartOrResumeThread")
		}
		if got := server.CallLog(t); !slices.Equal(got, []string{"thread/start"}) {
			t.Fatalf("call log = %v, want only thread/start (turn/start must never be sent)", got)
		}
	})
}

// TestCodexResumeUsesDurableThreadID proves §14.2: an existing
// ProviderBinding.SessionID drives thread/resume(threadId), never
// thread/start, and the durable ID is what gets used, not anything invented.
func TestCodexResumeUsesDurableThreadID(t *testing.T) {
	const cwd = "/workspace/task-1"
	server := startFakeCodexServer(t, []fakeCodexStep{
		fakeCodexThreadStartStep("existing-thread", cwd, "gpt-5-codex", "workspace-write", t),
	})
	defer server.Client.Close()

	cfg := CodexThreadConfig{CWD: cwd, Model: "gpt-5-codex", Sandbox: "workspace-write"}
	effective, err := codexStartOrResumeThread(context.Background(), server.Client, "existing-thread", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if effective.ThreadID != "existing-thread" {
		t.Fatalf("resumed thread ID = %q, want the durable %q", effective.ThreadID, "existing-thread")
	}
	if got := server.CallLog(t); !slices.Equal(got, []string{"thread/resume"}) {
		t.Fatalf("call log = %v, want thread/resume only (not thread/start)", got)
	}
}

// TestCodexEffectiveModelMismatchFails proves §14.3: Codex reporting a
// different effective model than the bound one fails closed — v1 has no
// provider model fallback.
func TestCodexEffectiveModelMismatchFails(t *testing.T) {
	server := startFakeCodexServer(t, []fakeCodexStep{
		fakeCodexThreadStartStep("thread-1", "/workspace", "gpt-4-different", "workspace-write", t),
	})
	defer server.Client.Close()

	cfg := CodexThreadConfig{CWD: "/workspace", Model: "gpt-5-codex", Sandbox: "workspace-write"}
	_, err := codexStartOrResumeThread(context.Background(), server.Client, "", cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v, want a model-mismatch rejection", err)
	}
}

// TestCodexSandboxMismatchFails proves §14.1: an effective sandbox wider
// than requested fails before any coding turn.
func TestCodexSandboxMismatchFails(t *testing.T) {
	server := startFakeCodexServer(t, []fakeCodexStep{
		fakeCodexThreadStartStep("thread-1", "/workspace", "gpt-5-codex", "danger-full-access", t),
	})
	defer server.Client.Close()

	cfg := CodexThreadConfig{CWD: "/workspace", Model: "gpt-5-codex", Sandbox: "read-only"}
	_, err := codexStartOrResumeThread(context.Background(), server.Client, "", cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "widens") {
		t.Fatalf("err = %v, want a sandbox-widening rejection", err)
	}
}

// TestCodexCWDWideningFails proves §14.1: an effective cwd different from
// what was requested fails before any coding turn — a different cwd can
// expose different files, so it is treated as a widening regardless of
// direction.
func TestCodexCWDWideningFails(t *testing.T) {
	server := startFakeCodexServer(t, []fakeCodexStep{
		fakeCodexThreadStartStep("thread-1", "/", "gpt-5-codex", "workspace-write", t),
	})
	defer server.Client.Close()

	cfg := CodexThreadConfig{CWD: "/workspace/task-1", Model: "gpt-5-codex", Sandbox: "workspace-write"}
	_, err := codexStartOrResumeThread(context.Background(), server.Client, "", cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("err = %v, want a cwd-mismatch rejection", err)
	}
}
