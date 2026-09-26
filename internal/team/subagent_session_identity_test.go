package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// Session identity rule and failure dispositions; the contract is the
// BackendBinding entry in docs/architecture/execution-runtime.md.

func anchorBoundEvent(t *testing.T, id, taskID, session string) RunEvent {
	t.Helper()
	return RunEvent{
		ID: id, Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: 1,
		IdempotencyKey: backendSessionBoundKey(taskID),
		Payload:        mustJSON(t, BackendSessionBoundPayload{TaskID: taskID, Attempt: 1, Backend: "codex", SessionID: session}),
	}
}

func legacyBoundEvent(t *testing.T, id, taskID string, attempt int, session string) RunEvent {
	t.Helper()
	return RunEvent{
		ID: id, Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: attempt,
		IdempotencyKey: fmt.Sprintf("backend-session-bound:%s:%d:%s", taskID, attempt, session),
		Payload:        mustJSON(t, BackendSessionBoundPayload{TaskID: taskID, Attempt: attempt, Backend: "codex", SessionID: session}),
	}
}

func bindingTransitionEvent(t *testing.T, id, taskID, session, world string) RunEvent {
	t.Helper()
	return RunEvent{
		ID: id, Type: string(EventTaskStarted), Actor: "coordinator", TaskID: taskID,
		Payload: mustJSON(t, map[string]any{"id": taskID, "backend_binding": BackendBinding{Backend: "codex", SessionID: session, ExecutionWorldID: world}}),
	}
}

// appendRebindingTransition appends a real task transition whose
// backend_binding names session, as a transition written from a live
// projection would.
func appendRebindingTransition(t *testing.T, c *Coordinator, taskID, session string) {
	t.Helper()
	item := cloneTodoItem(c.todoItemByID(taskID))
	target, err := c.sessionBindingTarget(taskID)
	if err != nil {
		t.Fatal(err)
	}
	item.BackendBinding = &BackendBinding{Backend: target.Backend, SessionID: session}
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventTaskStarted), Actor: "coordinator", TaskID: taskID, Payload: mustJSON(t, taskTransitionPayload(item))}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionIdentityRule(t *testing.T) {
	const task = "task-1"
	tests := []struct {
		name        string
		events      func(t *testing.T) []RunEvent
		wantSession string
		// wantConflictIDs are event IDs the conflict must name.
		wantConflictIDs []string
	}{
		{
			name: "no binding",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{{ID: "created", Type: string(EventTaskCreated), TaskID: task, Payload: mustJSON(t, map[string]any{"id": task})}}
			},
		},
		{
			name: "execution details may change after the anchor",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{
					anchorBoundEvent(t, "anchor", task, "S1"),
					bindingTransitionEvent(t, "started-2", task, "S1", "world-b"),
					legacyBoundEvent(t, "old-key", task, 3, "S1"),
				}
			},
			wantSession: "S1",
		},
		{
			name: "a transition may not rebind the anchored session",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{anchorBoundEvent(t, "anchor", task, "S1"), bindingTransitionEvent(t, "rebind", task, "S2", "")}
			},
			wantConflictIDs: []string{"anchor", "rebind"},
		},
		{
			name: "a later session event may not rebind the anchored session",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{anchorBoundEvent(t, "anchor", task, "S1"), legacyBoundEvent(t, "rebind", task, 2, "S2")}
			},
			wantConflictIDs: []string{"anchor", "rebind"},
		},
		{
			name: "legacy history without an anchor keeps the last binding",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{
					legacyBoundEvent(t, "old-1", task, 1, "S1"),
					legacyBoundEvent(t, "old-2", task, 2, "S2"),
					bindingTransitionEvent(t, "started-2", task, "S2", ""),
				}
			},
			wantSession: "S2",
		},
		{
			name: "a legacy provider_binding transition counts as a binding",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{{ID: "legacy-started", Type: string(EventTaskStarted), TaskID: task, Payload: mustJSON(t, map[string]any{
					"id": task, "provider_binding": ProviderBinding{Provider: "codex", SessionID: "S1"},
				})}}
			},
			wantSession: "S1",
		},
		{
			name: "the anchor freezes a legacy history",
			events: func(t *testing.T) []RunEvent {
				return []RunEvent{
					legacyBoundEvent(t, "old-1", task, 1, "S1"),
					legacyBoundEvent(t, "old-2", task, 2, "S2"),
					anchorBoundEvent(t, "anchor", task, "S2"),
					bindingTransitionEvent(t, "rebind", task, "S1", ""),
				}
			},
			wantConflictIDs: []string{"anchor", "rebind"},
		},
		{
			name: "other tasks and removals are ignored",
			events: func(t *testing.T) []RunEvent {
				removed := bindingTransitionEvent(t, "removed", task, "S9", "")
				removed.Type = "task_removed"
				return []RunEvent{
					anchorBoundEvent(t, "anchor", task, "S1"),
					bindingTransitionEvent(t, "other-task", "task-2", "S2", ""),
					removed,
				}
			},
			wantSession: "S1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sessionIdentityFromLineage(tt.events(t), task)
			if len(tt.wantConflictIDs) > 0 {
				requireIdentityConflict(t, err)
				for _, id := range tt.wantConflictIDs {
					if !strings.Contains(err.Error(), `"`+id+`"`) {
						t.Fatalf("conflict = %v, want it to name event %q", err, id)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("sessionIdentityFromLineage: %v", err)
			}
			gotSession := ""
			if got != nil {
				gotSession = got.Binding.SessionID
			}
			if gotSession != tt.wantSession {
				t.Fatalf("session = %q, want %q", gotSession, tt.wantSession)
			}
		})
	}
}

func TestSessionIdentityConflictBlocksLaunchButStillReplays(t *testing.T) {
	workspace := newCodexWorkspace(t)
	c, provider, item, rawLogPath := newCodexHarness(t, workspace, []fakeCodexStep{codexInitializeStep(t)})
	if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-anchor", "world-a")); err != nil {
		t.Fatal(err)
	}
	appendRebindingTransition(t, c, item.ID, "thread-other")

	_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "continue"))
	requireIdentityConflict(t, err)
	if got := classifyTaskFailure(err); got != FailureIdentityConflict {
		t.Fatalf("failure class = %q, want %q", got, FailureIdentityConflict)
	}
	if calls := readRawCalls(t, rawLogPath); len(calls) != 0 {
		t.Fatalf("app-server received %d calls, want none before the conflict", len(calls))
	}

	// Startup replay does not reject the contradiction, so recovery commands
	// that pass through it can still reach this task.
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayTodoList(events); err != nil {
		t.Fatalf("ReplayTodoList: %v", err)
	}
	if _, _, err := ReadCheckedActiveExecutionTaskEvidence(workspace); err != nil {
		t.Fatalf("ReadCheckedActiveExecutionTaskEvidence: %v", err)
	}
}

func TestSessionIdentityLegacyHistoryResumesLastSession(t *testing.T) {
	c, item := newSessionBindingCoordinator(t)
	for _, event := range []RunEvent{legacyBoundEvent(t, "", item.ID, 1, "thread-1"), legacyBoundEvent(t, "", item.ID, 2, "thread-2")} {
		if _, err := c.EventJournal().Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	appendRebindingTransition(t, c, item.ID, "thread-2")
	if got, err := c.resumableBackendSessionID(t.Context(), AttemptRequest{TaskID: item.ID}); err != nil || got != "thread-2" {
		t.Fatalf("resumable session = %q, %v; want the legacy history's last thread-2", got, err)
	}
	if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-2", "world-c")); err != nil {
		t.Fatalf("anchor the legacy session: %v", err)
	}
	appendRebindingTransition(t, c, item.ID, "thread-1")
	_, err := c.resumableBackendSessionID(t.Context(), AttemptRequest{TaskID: item.ID})
	requireIdentityConflict(t, err)
}

// invalidateEventStoreState makes the store's cached state invalid, as a
// failed sync does, so the next read must re-verify the file from disk.
func invalidateEventStoreState(t *testing.T, c *Coordinator) {
	t.Helper()
	configureEventStoreSyncFailureForEventType(t, c.eventStore, "worker_memory_confirmed", 1, errors.New("injected sync failure"))
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: "worker_memory_confirmed", Actor: "coordinator", Payload: []byte(`{"item_id":"x"}`)}); err == nil {
		t.Fatal("append with a failed sync succeeded")
	}
}

func eventLogPath(c *Coordinator) string {
	return filepath.Join(c.session.Workspace, logsDir, eventStoreFile)
}

func TestSessionIdentityFailureClassification(t *testing.T) {
	tests := []struct {
		name string
		// run produces the RunAttempt error from a real failure path.
		run  func(t *testing.T) error
		want TaskFailureClass
	}{
		{
			name: "resume answered with another thread",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
					codexInitializeStep(t),
					codexThreadResumeStep(t, "thread-other", workspace, codexSandboxWorkspaceWrite),
				})
				request := codexAttemptRequest(item, "continue")
				request.ProviderBinding = &ProviderBinding{Provider: "codex", SessionID: "thread-durable"}
				_, err := provider.RunAttempt(boundedAttemptContext(t), request)
				return err
			},
			want: FailureSessionResumeMismatch,
		},
		{
			name: "result repair resumed another thread",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				steps := codexRepairScript(t, workspace, "thread-repair", "", validProposalJSON(""), fakeCodexStep{}, fakeCodexStep{})
				steps[4] = codexThreadResumeStep(t, "thread-other", workspace, codexSandboxReadOnly)
				_, provider, item, _ := newCodexHarness(t, workspace, steps[:5])
				_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "do the work"))
				return err
			},
			want: FailureProtocol,
		},
		{
			name: "a writer the lineage read missed bound another session",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
					codexInitializeStep(t),
					codexThreadStartStep(t, "thread-new", workspace),
				})
				if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-anchor", "world-a")); err != nil {
					t.Fatal(err)
				}
				_ = c.taskTracker.TodoList().SetProviderBinding(item.ID, nil)
				_ = c.taskTracker.TodoList().SetBackendBinding(item.ID, nil)
				c.SetEventJournal(staleReadJournal{EventJournal: c.EventJournal()})
				_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
				return err
			},
			want: FailureIdentityConflict,
		},
		{
			name: "the durable session belongs to another backend",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{codexInitializeStep(t)})
				payload := mustJSON(t, BackendSessionBoundPayload{TaskID: item.ID, Attempt: 1, Backend: "other-agent", SessionID: "thread-1"})
				if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: item.ID, Attempt: 1, IdempotencyKey: backendSessionBoundKey(item.ID), Payload: payload}); err != nil {
					t.Fatal(err)
				}
				_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
				return err
			},
			want: FailureIdentityConflict,
		},
		{
			name: "the event log no longer verifies",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{codexInitializeStep(t)})
				if err := c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-1", "world-a")); err != nil {
					t.Fatal(err)
				}
				invalidateEventStoreState(t, c)
				data, err := os.ReadFile(eventLogPath(c))
				if err != nil {
					t.Fatal(err)
				}
				tampered := strings.Replace(string(data), `"session_id":"thread-1"`, `"session_id":"thread-X"`, 1)
				if tampered == string(data) {
					t.Fatal("the event log has no binding to tamper with")
				}
				if err := os.WriteFile(eventLogPath(c), []byte(tampered), 0o644); err != nil {
					t.Fatal(err)
				}
				_, err = provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
				return err
			},
			want: FailureIdentityConflict,
		},
		{
			name: "the event log cannot be opened",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{codexInitializeStep(t)})
				invalidateEventStoreState(t, c)
				if err := os.Remove(eventLogPath(c)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(eventLogPath(c), 0o755); err != nil {
					t.Fatal(err)
				}
				_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
				return err
			},
			want: FailureEnvironment,
		},
		{
			name: "the binding append failed",
			run: func(t *testing.T) error {
				workspace := newCodexWorkspace(t)
				c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
					codexInitializeStep(t),
					codexThreadStartStep(t, "thread-new", workspace),
				})
				c.SetEventJournal(&failingAppendJournal{EventJournal: c.EventJournal(), failType: EventBackendSessionBound})
				_, err := provider.RunAttempt(boundedAttemptContext(t), codexAttemptRequest(item, "start"))
				return err
			},
			want: FailureEnvironment,
		},
		{
			name: "the binding sync outcome is unknown",
			run: func(t *testing.T) error {
				c, item := newSessionBindingCoordinator(t)
				configureEventStoreSyncFailureForEventType(t, c.eventStore, string(EventBackendSessionBound), 1, errors.New("injected sync failure"))
				return c.persistProviderSessionBinding(t.Context(), item.ID, 1, codexSessionBinding("thread-1", "world-a"))
			},
			want: FailureEnvironment,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			if err == nil {
				t.Fatal("the failure path succeeded")
			}
			if got := classifyTaskFailure(err); got != tt.want {
				t.Fatalf("failure class = %q, want %q (error: %v)", got, tt.want, err)
			}
		})
	}
}

func TestSessionIdentityFailuresBlockForReconciliation(t *testing.T) {
	for _, class := range []TaskFailureClass{FailureSessionResumeMismatch, FailureIdentityConflict} {
		for _, replayable := range []bool{true, false} {
			disposition, _ := DecideRecovery(RecoveryDecisionInput{
				FailureClass: class, SideEffect: SideEffectWorkspaceWrite, Attempt: 1, MaxRetries: 3, Replayable: replayable,
			})
			if disposition != ReconcileOnly {
				t.Fatalf("DecideRecovery(%s, replayable=%v) = %s, want %s", class, replayable, disposition, ReconcileOnly)
			}
		}
		if got := SystemicDispositionForClass(class); got != string(NeedsHuman) {
			t.Fatalf("SystemicDispositionForClass(%s) = %q, want needs_human", class, got)
		}
	}
	if findings := validateOnFailureClasses("on-failure-classes", []TaskFailureClass{FailureSessionResumeMismatch, FailureIdentityConflict}); len(findings) != 0 {
		t.Fatalf("on-failure-classes findings = %#v, want both classes accepted", findings)
	}
}

// codexCallCount counts the recorded app-server calls of one method.
func codexCallCount(t *testing.T, rawLogPath, method string) int {
	t.Helper()
	count := 0
	for _, got := range rawCallMethods(t, readRawCalls(t, rawLogPath)) {
		if got == method {
			count++
		}
	}
	return count
}

func TestCodexResumeMismatchAfterSideEffectBlocksTask(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{
		Type:                 VerifyTaskResultAssert,
		TaskResultAssertions: []agent.TaskResultAssertion{{Pointer: "/summary", Op: "equals", Value: "DONE"}},
	}
	firstAttempt := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"success","summary":"not yet"}`)
	firstAttempt.WriteFile, firstAttempt.WriteFileContent = "side-effect.txt", "written by attempt 1"
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-1", workspace),
		firstAttempt,
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-other", workspace, codexSandboxWorkspaceWrite),
	}, worker, verify)
	rawLogPath := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	_, _ = c.executeTask(boundedAttemptContext(t), task, item.ID)

	got := c.todoItemByID(item.ID)
	if got.Status != TaskBlocked || got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureSessionResumeMismatch || got.FailureEvent.RetryDisposition != ReconcileOnly {
		t.Fatalf("task = status %s, failure %#v; want blocked by %s for reconciliation", got.Status, got.FailureEvent, FailureSessionResumeMismatch)
	}
	if resumes, starts, turns := codexCallCount(t, rawLogPath, codexMethodThreadResume), codexCallCount(t, rawLogPath, codexMethodThreadStart), codexCallCount(t, rawLogPath, codexMethodTurnStart); resumes != 1 || starts != 1 || turns != 1 {
		t.Fatalf("calls: resume=%d start=%d turn=%d, want one each and no attempt after the mismatch", resumes, starts, turns)
	}
	if _, err := os.Stat(filepath.Join(workspace, "side-effect.txt")); err != nil {
		t.Fatalf("attempt 1's side effect is gone: %v", err)
	}
}

func TestCodexIdentityConflictBlocksTaskWithoutLaunching(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c, item := newCodexEndToEndCoordinator(t, workspace, nil, worker, nil)
	rawLogPath := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
	payload, err := json.Marshal(BackendSessionBoundPayload{TaskID: item.ID, Attempt: 1, Backend: "other-agent", SessionID: "thread-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: item.ID, Attempt: 1, IdempotencyKey: backendSessionBoundKey(item.ID), Payload: payload}); err != nil {
		t.Fatal(err)
	}

	task := TaskDef{Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite, Execution: ExecutionContract{RequiresResult: true}}
	_, _ = c.executeTask(boundedAttemptContext(t), task, item.ID)

	got := c.todoItemByID(item.ID)
	if got.Status != TaskBlocked || got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureIdentityConflict {
		t.Fatalf("task = status %s, failure %#v; want blocked by %s", got.Status, got.FailureEvent, FailureIdentityConflict)
	}
	persisted, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "backend session identity needs reconciliation") {
		t.Fatalf("blocked task = %s, want the session identity reconciliation message", persisted)
	}
	if calls := readRawCalls(t, rawLogPath); len(calls) != 0 {
		t.Fatalf("app-server received %v, want no launch", rawCallMethods(t, calls))
	}
	if methods := rawCallMethods(t, readRawCalls(t, rawLogPath)); slices.Contains(methods, codexMethodThreadStart) {
		t.Fatalf("calls = %v, want no new session", methods)
	}
}
