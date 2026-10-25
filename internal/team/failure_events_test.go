package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// A structured failure event must reach every durable surface (todo item,
// reporter, event store, workspace projections, task journal) with the same
// class and disposition, redacted, and must replay from legacy, full and
// sparse payloads.

// failureEventCoordinator is a coordinator that emits task events to an
// event store from checkpoints.
func failureEventCoordinator(t *testing.T, name, shell string) (*Coordinator, *EventStore) {
	t.Helper()
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) {
		cfg.Name, cfg.Shell = name, shell
	}))
	c.projectDir = c.session.Workspace
	c.emittedTaskTransitions = make(map[string]bool)
	c.executionRunID = "run-" + name
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "session-"+name)
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = store // closed by the coordinator's cleanup
	return c, store
}

// failureEventSurfaces adds the checkpointed session, task journal and
// status recorder a cross-surface test inspects.
func failureEventSurfaces(t *testing.T, c *Coordinator) *[]StatusEvent {
	t.Helper()
	var reported []StatusEvent
	c.session.Dir = c.session.Workspace
	c.sessionData = NewSession()
	c.reportStatus = func(event StatusEvent) { reported = append(reported, event) }
	journal, err := openTaskJournal(c.session.Workspace)
	if err != nil {
		t.Fatalf("openTaskJournal: %v", err)
	}
	c.journal = journal // closed by the coordinator's cleanup
	return &reported
}

// TestFailureJournalRecordFallsBackToTodoListRunID covers a coordinator that
// runs without an execution run ID: the journal record takes the run ID the
// todo list carries instead of leaving the field empty.
func TestFailureJournalRecordFallsBackToTodoListRunID(t *testing.T) {
	c, _ := failureEventCoordinator(t, "fallback", "")
	failureEventSurfaces(t, c)
	c.executionRunID = ""
	c.taskTracker.TodoList().SetRunID("run-from-todo-list")
	c.recordTaskFailureWithEventAndOutput("worker", "do work", "worker failed", nil, "")
	journalData, err := readTaskJournalLog(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journalData), `"run_id":"run-from-todo-list"`) {
		t.Fatalf("journal failure record did not take the todo list run ID: %s", journalData)
	}
}

func failureEventPayloads(t *testing.T, store *EventStore, eventType, taskID string) []map[string]any {
	t.Helper()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.Type != eventType || (taskID != "" && event.TaskID != taskID) {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// assertFailureEventSurfacesRedacted checks that the workspace status
// projection, a task artifact and the task journal all carry the failure event
// and that none of them leaks secret. evidence lists further substrings one
// task artifact must carry alongside the failure event, such as the worker
// output a protocol failure has to keep.
func assertFailureEventSurfacesRedacted(t *testing.T, c *Coordinator, secret string, evidence ...string) {
	t.Helper()
	wantEvidence := append([]string{"Failure Event"}, evidence...)
	workspace := c.session.Workspace
	statusData, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusData), "failure_event:") || strings.Contains(string(statusData), secret) {
		t.Fatalf("workspace status projection = %s", statusData)
	}
	taskFiles, err := filepath.Glob(filepath.Join(workspace, tasksDir, c.session.Config.Name, "worker", "*.md"))
	if err != nil || len(taskFiles) == 0 {
		t.Fatalf("workspace task files = %v, err=%v", taskFiles, err)
	}
	foundEvent := false
	for _, taskFile := range taskFiles {
		taskData, err := os.ReadFile(taskFile)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(taskData), secret) {
			t.Fatalf("workspace task artifact leaked secret: %s", taskData)
		}
		carries := true
		for _, want := range wantEvidence {
			carries = carries && strings.Contains(string(taskData), want)
		}
		if carries {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("no workspace task artifact carries %q: %v", wantEvidence, taskFiles)
	}
	journalData, err := readTaskJournalLog(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journalData), `"failure_event"`) || !strings.Contains(string(journalData), `"task_id":"1"`) || strings.Contains(string(journalData), secret) {
		t.Fatalf("journal failure record = %s", journalData)
	}
}

func TestBlockedRecoveryFailureEventReachesEverySurface(t *testing.T) {
	c, store := failureEventCoordinator(t, "blocked-recovery", "sh")
	reported := failureEventSurfaces(t, c)
	c.taskTracker.TodoList().Restore([]*TodoItem{{
		ID: "1", Agent: "worker", Desc: "mutate api_token=task-secret", Status: TaskInProgress,
		SideEffect: SideEffectInfraMutation,
	}})

	if _, err := c.ResumeInterruptedTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked || item.FailureEvent == nil {
		t.Fatalf("blocked recovery item = %#v", item)
	}
	if item.FailureEvent.FailureClass != FailurePolicy || item.FailureEvent.Phase == "" || item.FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("blocked recovery event metadata = %#v", item.FailureEvent)
	}

	publicJSON, err := json.Marshal(FailureEventsFromTodos([]*TodoItem{item}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(publicJSON), `"failure_class":"policy"`) || strings.Contains(string(publicJSON), "task-secret") {
		t.Fatalf("public failure export = %s", publicJSON)
	}
	failureReported := false
	for _, event := range *reported {
		if event.Type != "failure" || event.TodoID != item.ID {
			continue
		}
		failureReported = true
		payload, ok := event.Data["failure_event"].(*FailureEventPayload)
		if !ok || payload.FailureClass != FailurePolicy || payload.RetryDisposition != NeedsHuman {
			t.Fatalf("reporter failure payload = %#v", event.Data)
		}
	}
	if !failureReported {
		t.Fatalf("missing structured failure reporter event: %#v", *reported)
	}

	c.emitTaskEventsFromCheckpoint([]*TodoItem{item})
	payloads := failureEventPayloads(t, store, "task_blocked", item.ID)
	if len(payloads) == 0 {
		t.Fatal("missing structured task_blocked event")
	}
	for _, payload := range payloads {
		if payload["failure_class"] != string(FailurePolicy) || payload["retry_disposition"] != string(NeedsHuman) || payload["failure_event"] == nil {
			t.Fatalf("blocked event-store payload = %#v", payload)
		}
		if raw, _ := json.Marshal(payload); strings.Contains(string(raw), "task-secret") {
			t.Fatalf("blocked event-store payload leaked task secret: %s", raw)
		}
	}
	assertFailureEventSurfacesRedacted(t, c, "task-secret")
}

func TestDAGAntiThrashingBlockPersistsStructuredFailureEvent(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}, maxConcurrent: 1}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "blocked scope", Kind: TaskKindOutcome, Advances: []string{"build"}}})[0]
	c.metricsMu.Lock()
	c.antiThrashing.reset()
	c.antiThrashing.HardBlocked = true
	c.antiThrashing.BlockedScopes[antiThrashingScopeKey("build", TaskKindOutcome)] = true
	c.metricsMu.Unlock()

	s := mustNewDAGScheduler(t, c, []TaskDef{{Agent: "worker", Goal: item.Desc, Kind: TaskKindOutcome, Advances: []string{"build"}}}, []*TodoItem{item}, nil)
	s.launchReady(context.Background())
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskBlocked || got.FailureEvent == nil {
		t.Fatalf("anti-thrashing DAG item = %#v", got)
	}
	if got.FailureEvent.FailureClass != FailurePolicy || got.FailureEvent.Phase == "" || got.FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("anti-thrashing DAG failure event = %#v", got.FailureEvent)
	}
}

func TestProtocolIncompleteFailureEventReachesEverySurface(t *testing.T) {
	c, store := failureEventCoordinator(t, "protocol-incomplete", "sh")
	c.session.Config.MaxRetries = 1
	// A worker without a model keeps admission off the provider backend.
	c.session.Agents["worker"] = &agent.AgentDef{Name: "worker", Role: "worker"}
	reported := failureEventSurfaces(t, c)
	task, item := createAdmittedTestTask(t, c, TaskDef{Agent: "worker", Goal: "process api_token=protocol-secret", Execution: ExecutionContract{RequiresResult: true}})
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	c.workerAgentOverride = scriptWorker(textTurn("worker output api_token=protocol-secret"))
	// No submit_result call: this forces the protocol-incomplete transition
	// and then lets the result-only repair path fail without another worker
	// replay.
	c.repairAgentOverride = &mockRepairAgent{}

	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("expected omitted submit_result to fail")
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureProtocol || got.FailureEvent.Phase == "" || got.FailureEvent.RetryDisposition == "" {
		t.Fatalf("protocol-incomplete live event = %#v", got.FailureEvent)
	}
	if !strings.Contains(got.Output, "worker output") || strings.Contains(got.Output, "protocol-secret") {
		t.Fatalf("protocol-incomplete todo output lost or leaked evidence: %q", got.Output)
	}

	publicJSON, err := json.Marshal(FailureEventsFromTodos([]*TodoItem{got}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(publicJSON), `"failure_class":"protocol"`) || strings.Contains(string(publicJSON), "protocol-secret") {
		t.Fatalf("protocol failure JSON = %s", publicJSON)
	}
	failureReported := false
	for _, event := range *reported {
		if event.Type != "failure" || event.TodoID != got.ID {
			continue
		}
		payload, ok := event.Data["failure_event"].(*FailureEventPayload)
		if !ok || payload.FailureClass != FailureProtocol || payload.Phase == "" || payload.RetryDisposition == "" {
			continue
		}
		failureOutput, hasOutput := event.Data["failure_output"].(string)
		if !hasOutput || !strings.Contains(failureOutput, "worker output") || strings.Contains(failureOutput, "protocol-secret") {
			continue
		}
		failureReported = true
		break
	}
	if !failureReported {
		t.Fatalf("missing protocol failure reporter event with redacted output: %#v", *reported)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var protocolPayload json.RawMessage
	for _, event := range events {
		if event.Type != "task_protocol_incomplete" || event.TaskID != got.ID {
			continue
		}
		protocolPayload = event.Payload
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["failure_class"] != string(FailureProtocol) || payload["retry_disposition"] == "" || payload["failure_event"] == nil {
			t.Fatalf("protocol event-store payload = %#v", payload)
		}
		if output, ok := payload["output"].(string); !ok || !strings.Contains(output, "worker output") || strings.Contains(output, "protocol-secret") {
			t.Fatalf("protocol event-store output = %#v", payload["output"])
		}
		if strings.Contains(string(event.Payload), "protocol-secret") {
			t.Fatalf("protocol event-store payload leaked secret: %s", event.Payload)
		}
	}
	if protocolPayload == nil {
		t.Fatal("missing task_protocol_incomplete event")
	}

	reduced := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: got.ID, Payload: []byte(`{"id":"1","desc":"protocol task","status":"pending"}`)},
		{Type: "task_protocol_incomplete", TaskID: got.ID, Payload: protocolPayload},
	})
	if len(reduced) != 1 || reduced[0].Status != TaskProtocolIncomplete || reduced[0].FailureEvent == nil || reduced[0].FailureEvent.FailureClass != FailureProtocol || reduced[0].Detail == "" {
		t.Fatalf("protocol event replay = %#v", reduced)
	}
	if !strings.Contains(reduced[0].Output, "worker output") || strings.Contains(reduced[0].Output, "protocol-secret") {
		t.Fatalf("protocol replay output = %q", reduced[0].Output)
	}
	assertFailureEventSurfacesRedacted(t, c, "protocol-secret", "worker output")
	journalData, err := readTaskJournalLog(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journalData), `"failure_output"`) || !strings.Contains(string(journalData), "worker output") {
		t.Fatalf("protocol journal lost worker evidence: %s", journalData)
	}
}

func TestFailureEventReducerSupportsLegacyFullAndSparsePayloads(t *testing.T) {
	tests := []struct {
		name       string
		eventType  string
		status     TaskStatus
		full       FailureEventPayload
		sparse     string
		legacy     string
		wantClass  TaskFailureClass
		wantSummry string
	}{
		{
			name:      "task_failed",
			eventType: "task_failed",
			status:    TaskError,
			full: FailureEventPayload{
				TaskID: "new", Phase: "execution", FailureClass: FailureExecution, RetryDisposition: RetryWorker,
				Command: "go test ./...", WorkDir: "/workspace", Shell: "sh", Stdout: "partial output",
				Fingerprint: "fp-1", Hint: "fix the failing test", Summary: "first failure",
			},
			sparse:     `{"id":"new","task_id":"new","status":"error","summary":"updated failure","failure_event":{"failure_class":"environment","summary":"updated failure"}}`,
			legacy:     `{"id":"legacy","desc":"legacy task description","status":"error","output":"legacy output"}`,
			wantClass:  FailureEnvironment,
			wantSummry: "updated failure",
		},
		{
			name:      "task_protocol_incomplete",
			eventType: "task_protocol_incomplete",
			status:    TaskProtocolIncomplete,
			full: FailureEventPayload{
				TaskID: "new", Phase: "protocol", FailureClass: FailureProtocol, RetryDisposition: ReconcileOnly,
				Command: "submit_result", WorkDir: "/workspace", Shell: "sh", Stdout: "partial output",
				Fingerprint: "fp-protocol", Hint: "submit the required result", Summary: "missing submit_result",
			},
			sparse:     `{"id":"new","status":"protocol_incomplete","failure_event":{"summary":"updated protocol summary"}}`,
			legacy:     `{"id":"legacy","desc":"legacy task description","status":"protocol_incomplete","output":"legacy output"}`,
			wantClass:  FailureProtocol,
			wantSummry: "updated protocol summary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fullPayload, err := json.Marshal(map[string]any{
				"id": "new", "task_id": "new", "status": string(tt.status), "summary": tt.full.Summary,
				"failure_event": tt.full,
			})
			if err != nil {
				t.Fatal(err)
			}
			reduced := ReduceToTodoList([]RunEvent{
				{Type: "task_created", TaskID: "new", Payload: []byte(`{"id":"new","desc":"new task description","status":"pending"}`)},
				{Type: tt.eventType, TaskID: "new", Payload: fullPayload},
				{Type: tt.eventType, TaskID: "new", Payload: []byte(tt.sparse)},
				{Type: tt.eventType, TaskID: "legacy", Payload: []byte(tt.legacy)},
			})
			if len(reduced) != 2 {
				t.Fatalf("reduced tasks = %d, want 2", len(reduced))
			}
			byID := map[string]*TodoItem{}
			for _, item := range reduced {
				byID[item.ID] = item
			}
			if legacy := byID["legacy"]; legacy.Status != tt.status || legacy.Desc != "legacy task description" {
				t.Fatalf("legacy replay = %#v", legacy)
			}
			got := byID["new"]
			if got.Status != tt.status || got.Desc != "new task description" || got.FailureEvent == nil {
				t.Fatalf("new task replay lost identity or event: %#v", got)
			}
			if got.FailureEvent.FailureClass != tt.wantClass || got.FailureEvent.Summary != tt.wantSummry {
				t.Fatalf("sparse fields did not update: %#v", got.FailureEvent)
			}
			if got.FailureEvent.Command != tt.full.Command || got.FailureEvent.Stdout != tt.full.Stdout || got.FailureEvent.Fingerprint != tt.full.Fingerprint {
				t.Fatalf("sparse replay erased omitted evidence: %#v", got.FailureEvent)
			}
		})
	}
}

func TestFailureEventIsSelfContainedAndBounded(t *testing.T) {
	c, store := failureEventCoordinator(t, "bounded", "bash")
	workspace := c.session.Workspace
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:  "worker",
		Desc:   strings.Repeat("do not persist this prompt ", 100),
		Verify: "test -f result.txt",
	}})[0]
	item.VerifyResult = &VerificationResult{Command: "test -f result.txt", WorkDir: workspace, ExitCode: 127, Stderr: "tool: not found"}

	detail := c.FailureDetail(errors.New("deliverable verification failed: tool not found"), "verification")
	c.PersistFailureWithDisposition("worker", item.Desc, item.ID, detail, ReplanRequired)
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())

	payloads := failureEventPayloads(t, store, "task_failed", "")
	if len(payloads) == 0 {
		t.Fatal("expected task_failed event")
	}
	failed := payloads[0]
	if _, ok := failed["desc"]; ok {
		t.Fatal("task_failed payload must not embed the full task description")
	}
	if failed["task_id"] != item.ID || failed["failure_class"] != string(FailureVerify) {
		t.Fatalf("identity/class payload = %#v", failed)
	}
	if failed["phase"] != "verification" || failed["retry_disposition"] != string(ReplanRequired) {
		t.Fatalf("phase/disposition payload = %#v", failed)
	}
	if failed["command"] != item.VerifyResult.Command || failed["work_dir"] != workspace || failed["shell"] != "bash" {
		t.Fatalf("command context payload = %#v", failed)
	}
	if failed["exit_code"] != float64(127) || failed["stderr"] != "tool: not found" {
		t.Fatalf("verification evidence payload = %#v", failed)
	}
	if failed["fingerprint"] == "" || failed["hint"] == "" {
		t.Fatalf("diagnostic payload missing fingerprint/hint: %#v", failed)
	}
	if summary, ok := failed["summary"].(string); !ok || len(summary) > 500 || strings.Contains(summary, "do not persist this prompt") {
		t.Fatalf("summary is not bounded/task-id based: %#v", failed["summary"])
	}
	if _, ok := failed["failure_event"]; !ok {
		t.Fatal("task_failed payload missing durable failure_event object")
	}
}

func TestStructuredEnvironmentClassSurvivesPersistence(t *testing.T) {
	c, store := failureEventCoordinator(t, "environment", "sh")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify missing tool"}})[0]
	item.Verify = "tool status"
	item.VerifyResult = &VerificationResult{Command: "tool status", WorkDir: c.session.Workspace, ExitCode: 127, Stderr: "tool: command not found"}
	detail := "deliverable verification failed: exit code 127"
	if strings.Contains(c.FailureDetail(errors.New(detail), "verification"), "failure_class=") {
		t.Fatal("FailureDetail must not inject a text-derived failure_class tag")
	}
	currentClass := ClassifyTaskFailureStructured(FailureClassificationInput{
		Err:             errors.New(detail),
		ExitCode:        item.VerifyResult.ExitCode,
		ExitCodeSource:  ExitCodeSourceVerify,
		ResolveFindings: environmentFindingsFromVerifyResult(item.VerifyResult),
	})
	if currentClass != FailureEnvironment {
		t.Fatalf("structured verifier classification = %q, want %q", currentClass, FailureEnvironment)
	}
	disposition, _ := DecideRecovery(RecoveryDecisionInput{
		FailureClass: currentClass, RecoveryPolicy: RecoveryRetry, Attempt: 1, MaxRetries: 3, EvidenceComplete: true, Replayable: true,
	})
	if disposition != ReplanRequired {
		t.Fatalf("environment recovery disposition = %q, want %q", disposition, ReplanRequired)
	}

	c.PersistFailureWithClass("worker", item.Desc, item.ID, detail, disposition, currentClass)
	got := c.taskTracker.TodoList().Items()[0]
	if got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureEnvironment {
		t.Fatalf("persisted failure event = %#v, want environment", got.FailureEvent)
	}
	if len(got.FailureFingerprints) != 1 {
		t.Fatalf("failure fingerprints = %#v, want one", got.FailureFingerprints)
	}
	wantFingerprint := NewFailureFingerprint("", "worker", stableOperation(got), FailureEnvironment, detail).Digest
	if got.FailureFingerprints[0].Digest != wantFingerprint {
		t.Fatalf("fingerprint digest = %q, want structured environment digest %q", got.FailureFingerprints[0].Digest, wantFingerprint)
	}
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
	payloads := failureEventPayloads(t, store, "task_failed", "")
	if len(payloads) == 0 {
		t.Fatal("expected task_failed event")
	}
	if payload := payloads[0]; payload["failure_class"] != string(FailureEnvironment) || payload["retry_disposition"] != string(ReplanRequired) {
		t.Fatalf("event class/disposition = %#v, want environment/replan_required", payload)
	}
}

func TestCheckpointTaskEventRetriesAfterAppendFailure(t *testing.T) {
	c, store := failureEventCoordinator(t, "append-retry", "")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
	item.Status = TaskError
	item.Detail = "execution failed"
	item.FailureEvent = &FailureEventPayload{
		TaskID: item.ID, Phase: "execution", FailureClass: FailureExecution, RetryDisposition: RetryNone, Summary: item.Detail,
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
	if c.DualWriteFailures() == 0 {
		t.Fatal("expected the closed event store to record a dual-write failure")
	}

	reopened, err := OpenEventStore(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = reopened // closed by the coordinator's cleanup
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
	payloads := failureEventPayloads(t, reopened, "task_failed", item.ID)
	if len(payloads) == 0 {
		t.Fatal("checkpoint task_failed event was not retried after append failure")
	}
}
