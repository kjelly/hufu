package team

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
