package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// Session binding identity (docs/tmp/spec.md PR 2): a task holds one backend
// session per branch. Every attempt resumes it; a different session is a
// conflict, and execution world, cwd, and turn are not part of the identity.

var bindingTestTarget = execution.ExecutionTarget{Backend: "codex", Model: "gpt-5-codex"}

// newSessionBindingCoordinator returns a coordinator with a real event store
// and one Codex-targeted task, enough to drive session binding without a
// Codex process.
func newSessionBindingCoordinator(t *testing.T) (*Coordinator, *TodoItem) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "binding-run", "binding-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "binding-test"}},
		projectDir:     workspace,
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		eventStore:     store,
		executionRunID: "binding-run",
		reportStatus:   func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "bind", Goal: "bind", ExecutionTarget: bindingTestTarget}})[0]
	return c, item
}

func codexSessionBinding(sessionID, worldID string) ProviderBinding {
	return ProviderBinding{Provider: "codex", Protocol: "app-server-v2", SessionID: sessionID, ExecutionWorldID: worldID, CWD: "/workspace", ResumeSupported: true}
}

// sessionBoundSessionIDs lists the session of every durable binding event.
func sessionBoundSessionIDs(t *testing.T, c *Coordinator) []string {
	t.Helper()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, event := range events {
		if binding, ok := durableSessionBindingFromEvent(event); ok {
			sessions = append(sessions, binding.Binding.SessionID)
		}
	}
	return sessions
}

func projectedSessionID(c *Coordinator, taskID string) string {
	item := c.todoItemByID(taskID)
	if item == nil || item.BackendBinding == nil {
		return ""
	}
	return item.BackendBinding.SessionID
}

func requireIdentityConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *ExecutionIdentityConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want an ExecutionIdentityConflictError", err)
	}
}

func TestPersistSessionBindingKeepsOneIdentityPerTask(t *testing.T) {
	type bind struct {
		attempt      int
		session      string
		world        string
		wantConflict bool
	}
	tests := []struct {
		name          string
		binds         []bind
		wantEvents    []string
		wantProjected string
	}{
		{
			// The retry loop restarts its attempt counter on every dispatch
			// and each attempt prepares a new execution world.
			name: "same session across attempts and worlds is idempotent",
			binds: []bind{
				{attempt: 1, session: "thread-1", world: "world-a"},
				{attempt: 2, session: "thread-1", world: "world-b"},
				{attempt: 1, session: "thread-1", world: "world-c"},
			},
			wantEvents:    []string{"thread-1"},
			wantProjected: "thread-1",
		},
		{
			name: "a second session is rejected",
			binds: []bind{
				{attempt: 1, session: "thread-1", world: "world-a"},
				{attempt: 2, session: "thread-2", world: "world-b", wantConflict: true},
			},
			wantEvents:    []string{"thread-1"},
			wantProjected: "thread-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, item := newSessionBindingCoordinator(t)
			for _, b := range tt.binds {
				err := c.persistProviderSessionBinding(t.Context(), item.ID, b.attempt, codexSessionBinding(b.session, b.world))
				if b.wantConflict {
					requireIdentityConflict(t, err)
				} else if err != nil {
					t.Fatalf("bind attempt %d %s: %v", b.attempt, b.session, err)
				}
			}
			if got := sessionBoundSessionIDs(t, c); !slices.Equal(got, tt.wantEvents) {
				t.Fatalf("durable bindings = %v, want %v", got, tt.wantEvents)
			}
			if got := projectedSessionID(c, item.ID); got != tt.wantProjected {
				t.Fatalf("projected session = %q, want %q", got, tt.wantProjected)
			}
		})
	}
}

func TestPersistSessionBindingSeesLegacyBindingEvents(t *testing.T) {
	tests := []struct {
		name  string
		event func(taskID string) RunEvent
	}{
		{
			name: "canonical event under the legacy per-attempt key",
			event: func(taskID string) RunEvent {
				payload, _ := json.Marshal(BackendSessionBoundPayload{TaskID: taskID, Attempt: 1, ExecutionTarget: bindingTestTarget, Backend: "codex", SessionID: "thread-1"})
				return RunEvent{Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: 1, IdempotencyKey: fmt.Sprintf("backend-session-bound:%s:1:thread-1", taskID), Payload: payload}
			},
		},
		{
			name: "legacy provider_session_bound event",
			event: func(taskID string) RunEvent {
				payload, _ := json.Marshal(ProviderSessionBoundPayload{TaskID: taskID, Attempt: 1, Provider: "codex", SessionID: "thread-1"})
				return RunEvent{Type: string(EventProviderSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: 1, Payload: payload}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, item := newSessionBindingCoordinator(t)
			if _, err := c.EventJournal().Append(t.Context(), tt.event(item.ID)); err != nil {
				t.Fatal(err)
			}
			requireIdentityConflict(t, c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-2", "world-b")))
			if err := c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-1", "world-b")); err != nil {
				t.Fatalf("rebinding the legacy session: %v", err)
			}
		})
	}
}

// staleReadJournal hides every event from reads, standing in for a writer
// that appended between the lineage read and the append.
type staleReadJournal struct{ EventJournal }

func (staleReadJournal) ReadEvents(context.Context) ([]RunEvent, error) { return nil, nil }

func TestPersistSessionBindingWriterLockCatchesUnseenBinding(t *testing.T) {
	c, item := newSessionBindingCoordinator(t)
	if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-1", "world-a")); err != nil {
		t.Fatal(err)
	}
	c.SetEventJournal(staleReadJournal{EventJournal: c.EventJournal()})

	requireIdentityConflict(t, c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-2", "world-b")))
	if got := sessionBoundSessionIDs(t, c); !slices.Equal(got, []string{"thread-1"}) {
		t.Fatalf("durable bindings = %v, want only thread-1", got)
	}
	if got := projectedSessionID(c, item.ID); got != "thread-1" {
		t.Fatalf("projected session = %q, want thread-1", got)
	}
}

// forkSessionBindingBranch forks "exp" from main at the current head.
func forkSessionBindingBranch(t *testing.T, c *Coordinator) (*SessionTree, string) {
	t.Helper()
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{ID: "fork-point", Actor: "coordinator", Type: "worker_memory_confirmed", Payload: []byte(`{"item_id":"fork-point"}`)}); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	branch, err := tree.CreateBranch("exp", "fork-point", c.eventStore)
	if err != nil {
		t.Fatal(err)
	}
	return tree, branch.ID
}

func checkoutSessionBindingBranch(t *testing.T, c *Coordinator, tree *SessionTree, branchID string) {
	t.Helper()
	tree.ActiveBranch = branchID
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	c.eventStore.SetBranchID(branchID)
}

func TestPersistSessionBindingIsBranchScoped(t *testing.T) {
	t.Run("a sibling binding after the fork does not leak", func(t *testing.T) {
		c, item := newSessionBindingCoordinator(t)
		tree, exp := forkSessionBindingBranch(t, c)
		if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-main", "world-a")); err != nil {
			t.Fatal(err)
		}
		checkoutSessionBindingBranch(t, c, tree, exp)
		if durable, err := c.durableBackendSessionBinding(t.Context(), item.ID); err != nil || durable != nil {
			t.Fatalf("exp durable binding = %#v, %v; want none from main after the fork", durable, err)
		}
		if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-exp", "world-b")); err != nil {
			t.Fatalf("bind on exp: %v", err)
		}
		checkoutSessionBindingBranch(t, c, tree, "main")
		requireIdentityConflict(t, c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-exp", "world-c")))
	})
	t.Run("a binding inherited through the fork is enforced", func(t *testing.T) {
		c, item := newSessionBindingCoordinator(t)
		if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-before", "world-a")); err != nil {
			t.Fatal(err)
		}
		tree, exp := forkSessionBindingBranch(t, c)
		checkoutSessionBindingBranch(t, c, tree, exp)
		requireIdentityConflict(t, c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-other", "world-b")))
		if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-before", "world-c")); err != nil {
			t.Fatalf("resume the inherited session on exp: %v", err)
		}
	})
}

// unreadableJournal fails every read, standing in for a degraded or
// unprojectable event log.
type unreadableJournal struct{ EventJournal }

func (unreadableJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	return nil, errors.New("injected unreadable event log")
}

func TestResumableBackendSessionID(t *testing.T) {
	tests := []struct {
		name string
		// prepare shapes the durable log and projection; it returns the
		// projected binding the attempt request carries.
		prepare     func(t *testing.T, c *Coordinator, taskID string) *ProviderBinding
		wantSession string
		wantErr     string
		wantClash   bool
	}{
		{
			name: "projected binding is used without reading the log",
			prepare: func(_ *testing.T, c *Coordinator, _ string) *ProviderBinding {
				c.SetEventJournal(unreadableJournal{EventJournal: c.EventJournal()})
				return &ProviderBinding{Provider: "codex", SessionID: "thread-projected"}
			},
			wantSession: "thread-projected",
		},
		{
			name:    "no binding anywhere starts a new session",
			prepare: func(*testing.T, *Coordinator, string) *ProviderBinding { return nil },
		},
		{
			name: "canonical binding covers a lagging projection",
			prepare: func(t *testing.T, c *Coordinator, taskID string) *ProviderBinding {
				if err := c.persistProviderSessionBinding(t.Context(), taskID, 1, codexSessionBinding("thread-1", "world-a")); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantSession: "thread-1",
		},
		{
			name: "an unreadable log fails closed",
			prepare: func(_ *testing.T, c *Coordinator, _ string) *ProviderBinding {
				c.SetEventJournal(unreadableJournal{EventJournal: c.EventJournal()})
				return nil
			},
			wantErr: "injected unreadable event log",
		},
		{
			name: "a durable session on another backend is a conflict",
			prepare: func(t *testing.T, c *Coordinator, taskID string) *ProviderBinding {
				payload := mustJSON(t, BackendSessionBoundPayload{TaskID: taskID, Attempt: 1, Backend: "other-agent", SessionID: "thread-1"})
				if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: 1, Payload: payload}); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantClash: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, item := newSessionBindingCoordinator(t)
			projected := tt.prepare(t, c, item.ID)
			got, err := c.resumableBackendSessionID(t.Context(), AttemptRequest{TaskID: item.ID, ProviderBinding: projected})
			switch {
			case tt.wantClash:
				requireIdentityConflict(t, err)
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			case err != nil:
				t.Fatalf("resumableBackendSessionID: %v", err)
			case got != tt.wantSession:
				t.Fatalf("session = %q, want %q", got, tt.wantSession)
			}
		})
	}
}

// TestSessionBindingUnknownSyncNeverOpensSecondSession covers an append whose
// write reached disk but whose sync failed. The outcome is unknown to the
// writer, so the next attempt must find that session instead of opening a
// second one.
func TestSessionBindingUnknownSyncNeverOpensSecondSession(t *testing.T) {
	c, item := newSessionBindingCoordinator(t)
	configureEventStoreSyncFailureForEventType(t, c.eventStore, string(EventBackendSessionBound), 1, errors.New("injected sync failure"))
	if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-1", "world-a")); err == nil {
		t.Fatal("bind with a failed sync succeeded")
	}
	if got := projectedSessionID(c, item.ID); got != "" {
		t.Fatalf("projected session = %q after an unknown append, want none", got)
	}
	if got, err := c.resumableBackendSessionID(t.Context(), AttemptRequest{TaskID: item.ID}); err != nil || got != "thread-1" {
		t.Fatalf("resumable session = %q, %v; want the thread-1 that reached disk", got, err)
	}
	requireIdentityConflict(t, c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-2", "world-b")))
	if err := c.persistProviderSessionBinding(t.Context(), item.ID, 2, codexSessionBinding("thread-1", "world-c")); err != nil {
		t.Fatalf("retry the same binding: %v", err)
	}
	if got := sessionBoundSessionIDs(t, c); !slices.Equal(got, []string{"thread-1"}) {
		t.Fatalf("durable bindings = %v, want only thread-1", got)
	}
}

func TestCodexResumeRejectsADifferentThread(t *testing.T) {
	const cwd = "/workspace/task-1"
	server := startFakeCodexServer(t, []fakeCodexStep{
		fakeCodexThreadStartStep("other-thread", cwd, "gpt-5-codex", "workspace-write", t),
	})
	defer server.Client.Close()

	bound := false
	cfg := CodexThreadConfig{CWD: cwd, Model: "gpt-5-codex", Sandbox: "workspace-write"}
	_, err := codexStartOrResumeThread(t.Context(), server.Client, "durable-thread", cfg, func(CodexEffectiveThreadState) error {
		bound = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "durable session") {
		t.Fatalf("error = %v, want a resumed-thread mismatch", err)
	}
	if bound {
		t.Fatal("a mismatched resume reached the binding callback")
	}
	if got := server.CallLog(t); !slices.Equal(got, []string{"thread/resume"}) {
		t.Fatalf("call log = %v, want thread/resume only", got)
	}
}

// boundedAttemptContext keeps a regression that reaches an unscripted
// app-server call from hanging the package until the test timeout.
func boundedAttemptContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCodexAttemptUsesCanonicalSessionBinding(t *testing.T) {
	t.Run("a lagging projection resumes the durable session", func(t *testing.T) {
		workspace := newCodexWorkspace(t)
		c, provider, item, rawLogPath := newCodexHarness(t, workspace, []fakeCodexStep{
			codexInitializeStep(t),
			codexThreadResumeStep(t, "thread-durable", workspace, codexSandboxWorkspaceWrite),
			fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
		})
		if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-durable", "world-a")); err != nil {
			t.Fatal(err)
		}
		// The event is durable but the projection lost it, as after a crash
		// or a failed projection update.
		if err := c.taskTracker.TodoList().SetProviderBinding(item.ID, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.taskTracker.TodoList().SetBackendBinding(item.ID, nil); err != nil {
			t.Fatal(err)
		}

		result, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "continue"))
		if err != nil {
			t.Fatalf("RunAttempt: %v", err)
		}
		if result.ProviderSessionID != "thread-durable" {
			t.Fatalf("session = %q, want the durable thread-durable", result.ProviderSessionID)
		}
		methods := rawCallMethods(t, readRawCalls(t, rawLogPath))
		if !slices.Contains(methods, codexMethodThreadResume) || slices.Contains(methods, codexMethodThreadStart) {
			t.Fatalf("calls = %v, want thread/resume and no thread/start", methods)
		}
	})
	t.Run("an unreadable log fails before the app-server starts", func(t *testing.T) {
		workspace := newCodexWorkspace(t)
		c, provider, item, rawLogPath := newCodexHarness(t, workspace, []fakeCodexStep{codexInitializeStep(t)})
		c.SetEventJournal(unreadableJournal{EventJournal: c.EventJournal()})

		_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
		if err == nil || !strings.Contains(err.Error(), "resolve durable Codex session") {
			t.Fatalf("RunAttempt error = %v, want a durable-session resolution failure", err)
		}
		if calls := readRawCalls(t, rawLogPath); len(calls) != 0 {
			t.Fatalf("app-server received %d calls, want none", len(calls))
		}
	})
	t.Run("a failed binding append never starts a turn", func(t *testing.T) {
		workspace := newCodexWorkspace(t)
		c, provider, item, rawLogPath := newCodexHarness(t, workspace, []fakeCodexStep{
			codexInitializeStep(t),
			codexThreadStartStep(t, "thread-new", workspace),
			fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
		})
		c.SetEventJournal(&failingAppendJournal{EventJournal: c.EventJournal(), failType: EventBackendSessionBound})

		if _, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start")); err == nil {
			t.Fatal("RunAttempt succeeded after its binding append failed")
		}
		methods := rawCallMethods(t, readRawCalls(t, rawLogPath))
		if !slices.Contains(methods, codexMethodThreadStart) || slices.Contains(methods, codexMethodTurnStart) {
			t.Fatalf("calls = %v, want thread/start and no turn/start", methods)
		}
		if got := projectedSessionID(c, item.ID); got != "" {
			t.Fatalf("projected session = %q after a failed append, want none", got)
		}
	})
}
