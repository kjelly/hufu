package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestBindActiveMutationTargetRejectsStaleAttemptAndCompletedTask(t *testing.T) {
	workspace := writeSessionFacadeFixture(t, &team.TodoItem{
		ID: "task-1", Status: team.TaskBlocked, Recovery: team.RecoveryRetry,
		SideEffect: team.SideEffectWorkspaceWrite, MaxRetries: 3, Retries: 1,
	})
	options := &sessionFacadeOptions{team: "fixture-team", run: "run-1", branch: "main", task: "task-1", attempt: 1}
	_, err := bindActiveMutationTarget(t.Context(), workspace, options, true)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale attempt error = %v", err)
	}

	workspace = writeSessionFacadeFixture(t, &team.TodoItem{ID: "task-1", Status: team.TaskDone, Recovery: team.RecoveryRetry})
	options.attempt = 0
	_, err = bindActiveMutationTarget(t.Context(), workspace, options, true)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("completed task error = %v", err)
	}
}

func TestBindActiveMutationTargetAcceptsExactCurrentRetry(t *testing.T) {
	workspace := writeSessionFacadeFixture(t, &team.TodoItem{
		ID: "task-1", Status: team.TaskBlocked, Recovery: team.RecoveryRetry,
		SideEffect: team.SideEffectWorkspaceWrite, MaxRetries: 3,
	})
	options := &sessionFacadeOptions{team: "fixture-team", run: "run-1", branch: "main", task: "task-1", attempt: 1}
	target, err := bindActiveMutationTarget(t.Context(), workspace, options, true)
	if err != nil {
		t.Fatal(err)
	}
	if target.eligibility == nil || !target.eligibility.RetryEligible {
		t.Fatalf("eligibility = %#v", target.eligibility)
	}
}

func TestSessionResumeRejectsTerminalSessionAsStale(t *testing.T) {
	session := &team.SessionData{Tasks: []*team.TodoItem{{ID: "done", Status: team.TaskDone}}}
	if sessionHasResumableWork(session) {
		t.Fatal("terminal session was resumable")
	}
}

func TestSessionResumeAcceptsReplanRequiredAndChangesPrompt(t *testing.T) {
	session := &team.SessionData{Tasks: []*team.TodoItem{{
		ID: "budget", Status: team.TaskError,
		FailureEvent: &team.FailureEventPayload{RetryDisposition: team.ReplanRequired},
	}}}
	if !sessionHasResumableWork(session) {
		t.Fatal("replan-required session was not resumable")
	}
	prompt := resumeInstruction(session)
	if !strings.Contains(prompt, "budget") || !strings.Contains(prompt, "materially changed plan") || !strings.Contains(prompt, "do not replay") {
		t.Fatalf("resume prompt = %q", prompt)
	}
}

func TestBindActiveMutationTargetRejectsHistoricalBranch(t *testing.T) {
	workspace := writeSessionFacadeFixture(t, &team.TodoItem{
		ID: "task-1", Status: team.TaskBlocked, Recovery: team.RecoveryRetry,
		SideEffect: team.SideEffectWorkspaceWrite, MaxRetries: 3,
	})
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := tree.CreateBranch("active-copy", "main", store)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = branch.ID
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	options := &sessionFacadeOptions{team: "fixture-team", run: "run-1", branch: "main", task: "task-1", attempt: 1}
	_, err = bindActiveMutationTarget(t.Context(), workspace, options, true)
	if err == nil || !strings.Contains(err.Error(), "historical") {
		t.Fatalf("historical branch error = %v", err)
	}
}

func writeSessionFacadeFixture(t *testing.T, item *team.TodoItem) string {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, appendErr := store.AppendPersisted(event); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: mustJSON(t, map[string]any{"scope": map[string]string{"team_id": "fixture-team"}})})
	appendEvent(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: item.ID, Payload: mustJSON(t, item)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(workspace, &team.SessionData{
		ActiveRunInputSnapshotID: "input-1",
		RunInputSnapshots:        []team.RunInputSnapshot{{ID: "input-1", RunID: "run-1"}},
		Tasks:                    []*team.TodoItem{item},
	}); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
