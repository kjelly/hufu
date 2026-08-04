package team

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestProjectAgentStatusesUsesCanonicalTaskAndTerminalState(t *testing.T) {
	items := []*TodoItem{
		{ID: "1", Agent: "Worker", Status: TaskDone},
		{ID: "2", Agent: "worker", Status: TaskError},
		{ID: "3", Agent: "paused-agent", Status: TaskInProgress},
		{ID: "4", Agent: "resume-agent", Status: TaskDone},
		{ID: "5", Agent: "idle-agent", Status: TaskPending},
	}
	sessions := []TerminalSession{
		{OwnerTaskID: "3", Agent: "paused-agent", Running: true, State: TerminalSessionRunning, Controller: TerminalControllerUser},
		{OwnerTaskID: "4", Agent: "resume-agent", Running: false, State: TerminalSessionUnknown},
	}

	got := ProjectAgentStatuses(items, sessions)
	want := map[string]AgentStatus{
		"worker":       AgentStatusError,
		"paused-agent": AgentStatusPaused,
		"resume-agent": AgentStatusError,
		"idle-agent":   AgentStatusIdle,
	}
	if len(got) != len(want) {
		t.Fatalf("projected statuses = %#v, want %#v", got, want)
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("status[%q] = %q, want %q", name, got[name], expected)
		}
	}
}

func TestProjectAgentStatusesDistinguishesContainedAndManualTerminalCleanup(t *testing.T) {
	items := []*TodoItem{
		{ID: "contained", Agent: "contained", Status: TaskDone},
		{ID: "manual", Agent: "manual", Status: TaskDone},
	}
	sessions := []TerminalSession{
		{ID: "contained-session", OwnerTaskID: "contained", Agent: "contained", State: TerminalSessionClosed, CleanupState: TerminalCleanupCompleted},
		{ID: "manual-session", OwnerTaskID: "manual", Agent: "manual", State: TerminalSessionClosed, CleanupState: TerminalCleanupManual},
	}
	got := ProjectAgentStatuses(items, sessions)
	if got["contained"] != AgentStatusIdle {
		t.Fatalf("contained terminal status = %q, want idle", got["contained"])
	}
	if got["manual"] != AgentStatusError {
		t.Fatalf("manual cleanup terminal status = %q, want error", got["manual"])
	}

	workspace := t.TempDir()
	if err := ReconcileAgentStatuses(workspace, items, sessions); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "contained.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "automatically contained; safe to retry") {
		t.Fatalf("contained cleanup guidance missing: %s", data)
	}
	data, err = os.ReadFile(filepath.Join(workspace, statusDir, "manual.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "requires manual intervention; do not retry") {
		t.Fatalf("manual cleanup guidance missing: %s", data)
	}
}

func TestReconcileAgentStatusesKeepsDetailAndFailureEventFromSameGoverningTask(t *testing.T) {
	workspace := t.TempDir()
	earlier := time.Now().Add(-time.Minute)
	later := time.Now()
	items := []*TodoItem{
		{ID: "1", Agent: "Helper", Status: TaskDone, Detail: "Task completed successfully. Deliverables verified.", EndedAt: earlier},
		{ID: "2", Agent: "helper", Status: TaskError, Detail: "verification failed", EndedAt: later, FailureEvent: &FailureEventPayload{TaskID: "2", Phase: "verification", FailureClass: FailureVerify, RetryDisposition: RetryNone, Summary: "artifact missing"}},
	}
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "helper.yml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "Task completed successfully") || !strings.Contains(content, "detail: verification failed") || !strings.Contains(content, "artifact missing") {
		t.Fatalf("error projection combined stale and current evidence: %q", content)
	}

	items[1].Status = TaskDone
	items[1].Detail = "recovered successfully"
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(workspace, statusDir, "helper.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if content = string(data); strings.Contains(content, "failure_event:") || strings.Contains(content, "artifact missing") {
		t.Fatalf("idle projection retained stale failure state: %q", content)
	}
}

// TestReconcileAgentStatusesCurrentWorkOutranksOlderReplannedFailure pins the
// fix for an agent whose earlier task errored and was replanned (not
// retried) while it moved on to a new, currently in-progress task: the
// projection must read the agent's current activity, not get stuck showing
// the old task's error forever just because "error" used to always outrank
// "working" once emitted for that agent name.
func TestReconcileAgentStatusesCurrentWorkOutranksOlderReplannedFailure(t *testing.T) {
	workspace := t.TempDir()
	earlier := time.Now().Add(-time.Hour)
	later := time.Now()
	items := []*TodoItem{
		{
			ID: "6", Agent: "helper", Status: TaskError, EndedAt: earlier,
			Detail: `terminal command "pilot edit" for task 6 exited with status -1`,
			FailureEvent: &FailureEventPayload{
				TaskID: "6", Phase: "execution", FailureClass: FailureExecution,
				RetryDisposition: RetryNone, Summary: "terminal exited with status -1",
			},
		},
		{ID: "8", Agent: "helper", Status: TaskInProgress, StartedAt: later, Detail: "retry 1/2 — continuing from previous progress"},
	}
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "helper.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded projectedStatusRecord
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("projected status is not valid YAML: %v; data=%q", err, data)
	}
	if decoded.Status != AgentStatusWorking {
		t.Fatalf("status = %q, want working (task 8 is active); got record %#v", decoded.Status, decoded)
	}
	if decoded.FailureEvent != nil {
		t.Fatalf("current-status failure_event must belong to the active task, not task 6: %#v", decoded.FailureEvent)
	}
	if decoded.UnresolvedFailure == nil || decoded.UnresolvedFailure.TaskID != "6" {
		t.Fatalf("unresolved_failure = %#v, want task 6's failure preserved as separate evidence", decoded.UnresolvedFailure)
	}
}

func TestReconcileAgentStatusesManualCleanupGuidanceOverridesEarlierCompletedSession(t *testing.T) {
	workspace := t.TempDir()
	items := []*TodoItem{{ID: "task", Agent: "worker", Status: TaskDone}}
	sessions := []TerminalSession{
		{ID: "contained-first", OwnerTaskID: "task", Agent: "worker", State: TerminalSessionClosed, CleanupState: TerminalCleanupCompleted},
		{ID: "manual-second", OwnerTaskID: "task", Agent: "worker", State: TerminalSessionUnknown, CleanupState: TerminalCleanupManual},
	}
	if err := ReconcileAgentStatuses(workspace, items, sessions); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "contained-first was automatically contained; safe to retry") {
		t.Fatalf("contained guidance masked manual intervention: %s", data)
	}
	if !strings.Contains(string(data), "manual-second requires manual intervention; do not retry") {
		t.Fatalf("manual-intervention guidance missing: %s", data)
	}
}

func TestReconcileAgentStatusesRedactsAndSerializesDetailAsYAML(t *testing.T) {
	workspace := t.TempDir()
	detail := "request failed\nnext: line\napi_token=super-secret-value"
	items := []*TodoItem{{ID: "1", Agent: "worker", Status: TaskError, Detail: detail}}
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || string(data) == detail || containsSecret(string(data), "super-secret-value") {
		t.Fatalf("detail was not safely persisted: %q", data)
	}
	var decoded projectedStatusRecord
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("projected status is not valid YAML: %v; data=%q", err, data)
	}
	if decoded.Status != AgentStatusError {
		t.Fatalf("decoded status = %q, want error", decoded.Status)
	}
	if decoded.Detail != "request failed\nnext: line\napi_token=[REDACTED]" {
		t.Fatalf("decoded detail = %q, want redacted multiline detail", decoded.Detail)
	}
}

func TestPersistFailureProjectsStructuredRedactedFailureEvent(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "sensitive prompt that must not be projected"}})[0]
	item.VerifyResult = &VerificationResult{
		Command:  "curl -H 'Authorization: Bearer token-secret-value'",
		WorkDir:  "/project/api_token=path-secret-value",
		ExitCode: 7,
		Stdout:   "api_token=stdout-secret-value",
		Stderr:   "password=stderr-secret-value",
	}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: tracker,
	}
	c.PersistFailureWithClass("worker", item.Desc, item.ID, "source=verification | error=password=detail-secret-value", RetryNone, FailureVerify)

	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var projected projectedStatusRecord
	if err := yaml.Unmarshal(data, &projected); err != nil {
		t.Fatalf("status projection is not valid YAML: %v; data=%q", err, data)
	}
	if projected.Status != AgentStatusError || projected.FailureEvent == nil {
		t.Fatalf("projected status = %#v, want error with failure event", projected)
	}
	if projected.FailureEvent.FailureClass != FailureVerify || projected.FailureEvent.TaskID != item.ID {
		t.Fatalf("projected failure event = %#v", projected.FailureEvent)
	}
	for _, secret := range []string{"token-secret-value", "path-secret-value", "stdout-secret-value", "stderr-secret-value", "detail-secret-value"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret %q leaked into projected status: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), "failure_class: verification") || !strings.Contains(string(data), "stderr: password=[REDACTED]") {
		t.Fatalf("structured failure evidence missing from projected status: %s", data)
	}
}

func TestReconcileAgentStatusesRejectsPathTraversalAgentNames(t *testing.T) {
	for _, name := range []string{"../outside", "/tmp/outside", `..\outside`, "worker/child"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			err := ReconcileAgentStatuses(workspace, []*TodoItem{{ID: "1", Agent: name, Status: TaskDone}}, nil)
			if err == nil || !strings.Contains(err.Error(), "unsafe projected status filename") {
				t.Fatalf("traversal name error = %v, want unsafe filename rejection", err)
			}
			if _, statErr := os.Stat(filepath.Join(workspace, "outside.yml")); !os.IsNotExist(statErr) {
				t.Fatalf("unexpected outside status file, stat error = %v", statErr)
			}
		})
	}
}

func containsSecret(content, secret string) bool {
	for _, candidate := range []string{secret, "super-secret-value"} {
		if candidate != "" && strings.Contains(content, candidate) {
			return true
		}
	}
	return false
}

func TestReconcileAgentStatusesRepairsStaleFilesAndIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	statusPath := filepath.Join(workspace, statusDir, "worker.yml")
	stalePath := filepath.Join(workspace, statusDir, "stale.yml")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte("status: working\ntask: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("status: working\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []*TodoItem{{ID: "1", Agent: "worker", Desc: "new task", Status: TaskDone}}
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "status: idle\n" {
		t.Fatalf("projected file = %q, want canonical idle status", first)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale status still exists, stat error = %v", err)
	}
	if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("projection is not idempotent: first %q, second %q", first, second)
	}
}

func TestReconcileAgentStatusesHandlesEmptyAndCrashResumeStates(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, statusDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, statusDir, "worker.yml"), []byte("status: working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []*TodoItem{{ID: "1", Agent: "worker", Status: TaskPaused}}
	if err := ReconcileAgentStatuses(workspace, items, []TerminalSession{{OwnerTaskID: "1", State: TerminalSessionUnknown}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "status: error\n" {
		t.Fatalf("crash-resume projection = %q, want fail-closed error", data)
	}
	if err := ReconcileAgentStatuses(workspace, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, statusDir, "worker.yml")); !os.IsNotExist(err) {
		t.Fatalf("empty canonical state left status file, stat error = %v", err)
	}
}

func TestCoordinatorProjectionIncludesConfiguredIdleWorkersWithoutTodoMutation(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker"},
			},
		},
		taskTracker: NewTaskTracker(),
	}
	items := c.taskTracker.TodoList().Items()
	if err := c.reconcileProjectedItems(items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("projection mutated canonical input slice: got %d items", len(items))
	}
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "status: idle\n" {
		t.Fatalf("configured idle worker projection = %q, want idle", data)
	}
}

func TestReconcileAgentStatusesSerializesConcurrentSnapshots(t *testing.T) {
	workspace := t.TempDir()
	items := []*TodoItem{{ID: "1", Agent: "worker", Status: TaskInProgress}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ReconcileAgentStatuses(workspace, items, nil); err != nil {
				t.Errorf("concurrent projection: %v", err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded projectedStatusRecord
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("final projection is invalid YAML: %v; data=%q", err, data)
	}
	if decoded.Status != AgentStatusWorking {
		t.Fatalf("final status = %q, want working", decoded.Status)
	}
	entries, err := os.ReadDir(filepath.Join(workspace, statusDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Fatalf("temporary projection file leaked: %s", entry.Name())
		}
	}
}
