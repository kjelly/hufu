package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestWP13BlockedRecoveryFailureEventAcrossSurfaces(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-wp13-blocked", "session-wp13-blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()

	var reported []StatusEvent
	c := &Coordinator{
		session:                &TeamSession{Workspace: workspace, Dir: workspace, Config: agent.TeamConfig{Name: "wp13-blocked", Shell: "sh"}},
		sessionData:            NewSession(),
		projectDir:             workspace,
		eventStore:             es,
		journal:                journal,
		taskTracker:            NewTaskTracker(),
		reportStatus:           func(event StatusEvent) { reported = append(reported, event) },
		emittedTaskTransitions: make(map[string]bool),
	}
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
	for _, event := range reported {
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
		t.Fatalf("missing structured failure reporter event: %#v", reported)
	}

	c.emitTaskEventsFromCheckpoint([]*TodoItem{item})
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	foundBlocked := false
	for _, event := range events {
		if event.Type != "task_blocked" || event.TaskID != item.ID {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["failure_class"] != string(FailurePolicy) || payload["retry_disposition"] != string(NeedsHuman) || payload["failure_event"] == nil {
			t.Fatalf("blocked event-store payload = %#v", payload)
		}
		if strings.Contains(string(event.Payload), "task-secret") {
			t.Fatalf("blocked event-store payload leaked task secret: %s", event.Payload)
		}
		foundBlocked = true
	}
	if !foundBlocked {
		t.Fatal("missing structured task_blocked event")
	}

	statusData, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusData), "failure_event:") || strings.Contains(string(statusData), "task-secret") {
		t.Fatalf("workspace status projection = %s", statusData)
	}
	taskFiles, err := filepath.Glob(filepath.Join(workspace, tasksDir, "wp13-blocked", "worker", "*.md"))
	if err != nil || len(taskFiles) != 1 {
		t.Fatalf("workspace task files = %v, err=%v", taskFiles, err)
	}
	taskData, err := os.ReadFile(taskFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskData), "Failure Event") || strings.Contains(string(taskData), "task-secret") {
		t.Fatalf("workspace task artifact = %s", taskData)
	}
	journalData, err := os.ReadFile(filepath.Join(workspace, logsDir, taskJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journalData), `"failure_event"`) || !strings.Contains(string(journalData), `"task_id":"1"`) {
		t.Fatalf("journal failure record = %s", journalData)
	}
}

func TestWP13DAGAntiThrashingBlockPersistsStructuredFailureEvent(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}, maxConcurrent: 1}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "blocked scope", Kind: TaskKindOutcome, Advances: []string{"build"}}})[0]
	c.metricsMu.Lock()
	c.antiThrashing.reset()
	c.antiThrashing.HardBlocked = true
	c.antiThrashing.BlockedScopes[antiThrashingScopeKey("build", TaskKindOutcome)] = true
	c.metricsMu.Unlock()

	s := newDAGScheduler(c, []TaskDef{{Agent: "worker", Goal: item.Desc, Kind: TaskKindOutcome, Advances: []string{"build"}}}, []*TodoItem{item}, nil)
	s.launchReady(context.Background())
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskBlocked || got.FailureEvent == nil {
		t.Fatalf("anti-thrashing DAG item = %#v", got)
	}
	if got.FailureEvent.FailureClass != FailurePolicy || got.FailureEvent.Phase == "" || got.FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("anti-thrashing DAG failure event = %#v", got.FailureEvent)
	}
}

func TestWP13ProtocolIncompleteFailureEventAcrossSurfaces(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-wp13-protocol", "session-wp13-protocol")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()

	var reported []StatusEvent
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace, Dir: workspace,
			Config: agent.TeamConfig{Name: "wp13-protocol", Shell: "sh", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}},
		},
		sessionData:            NewSession(),
		projectDir:             workspace,
		taskTracker:            NewTaskTracker(),
		eventStore:             es,
		journal:                journal,
		reportStatus:           func(event StatusEvent) { reported = append(reported, event) },
		emittedTaskTransitions: make(map[string]bool),
		taskResultCache:        make(map[string][]cachedTaskEntry),
		executionRunID:         "run-wp13-protocol",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "process api_token=protocol-secret"}})[0]
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	c.workerAgentOverride = &mockWorkerTextAgent{text: "worker output api_token=protocol-secret"}
	// No submit_result call: this forces the protocol-incomplete transition and
	// then lets the result-only repair path fail without another worker replay.
	c.repairAgentOverride = &mockRepairAgent{}

	_, err = c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: item.Desc,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
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
	for _, event := range reported {
		if event.Type != "failure" || event.TodoID != got.ID {
			continue
		}
		failureReported = true
		payload, ok := event.Data["failure_event"].(*FailureEventPayload)
		if !ok || payload.FailureClass != FailureProtocol || payload.Phase == "" || payload.RetryDisposition == "" {
			failureReported = false
			continue
		}
		failureOutput, hasOutput := event.Data["failure_output"].(string)
		if !hasOutput || !strings.Contains(failureOutput, "worker output") || strings.Contains(failureOutput, "protocol-secret") {
			continue
		}
		break
	}
	if !failureReported {
		t.Fatalf("missing protocol failure reporter event: %#v", reported)
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	foundProtocol := false
	var protocolPayload json.RawMessage
	for _, event := range events {
		if event.Type != "task_protocol_incomplete" || event.TaskID != got.ID {
			continue
		}
		foundProtocol = true
		protocolPayload = event.Payload
		var payload map[string]interface{}
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
	if !foundProtocol {
		t.Fatal("missing task_protocol_incomplete event")
	}

	reduced := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: got.ID, Payload: []byte(`{"id":"1","desc":"protocol task","status":"pending"}`)}, {Type: "task_protocol_incomplete", TaskID: got.ID, Payload: protocolPayload}})
	if len(reduced) != 1 || reduced[0].Status != TaskProtocolIncomplete || reduced[0].FailureEvent == nil || reduced[0].FailureEvent.FailureClass != FailureProtocol || reduced[0].Detail == "" {
		t.Fatalf("protocol event replay = %#v", reduced)
	}
	if !strings.Contains(reduced[0].Output, "worker output") || strings.Contains(reduced[0].Output, "protocol-secret") {
		t.Fatalf("protocol replay output = %q", reduced[0].Output)
	}

	statusData, err := os.ReadFile(filepath.Join(workspace, statusDir, "worker.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusData), "failure_event:") || strings.Contains(string(statusData), "protocol-secret") {
		t.Fatalf("protocol status projection = %s", statusData)
	}
	taskFiles, err := filepath.Glob(filepath.Join(workspace, tasksDir, "wp13-protocol", "worker", "*.md"))
	if err != nil || len(taskFiles) == 0 {
		t.Fatalf("protocol task files = %v, err=%v", taskFiles, err)
	}
	foundTaskEvidence := false
	for _, taskFile := range taskFiles {
		taskData, readErr := os.ReadFile(taskFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(taskData), "protocol-secret") {
			t.Fatalf("protocol task artifact leaked secret: %s", taskData)
		}
		if strings.Contains(string(taskData), "Failure Event") && strings.Contains(string(taskData), "worker output") {
			foundTaskEvidence = true
		}
	}
	if !foundTaskEvidence {
		t.Fatalf("protocol task artifacts lost worker evidence: %v", taskFiles)
	}
	journalData, err := os.ReadFile(filepath.Join(workspace, logsDir, taskJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journalData), `"failure_event"`) || !strings.Contains(string(journalData), `"failure_output"`) || !strings.Contains(string(journalData), "worker output") || strings.Contains(string(journalData), "protocol-secret") || !strings.Contains(string(journalData), `"task_id":"1"`) {
		t.Fatalf("protocol journal = %s", journalData)
	}
}

func TestWP13ProtocolIncompleteReducerSupportsLegacyNewAndSparsePayloads(t *testing.T) {
	full := FailureEventPayload{
		TaskID: "new", Phase: "protocol", FailureClass: FailureProtocol,
		RetryDisposition: ReconcileOnly, Command: "submit_result", WorkDir: "/workspace",
		Shell: "sh", Fingerprint: "fp-protocol", Hint: "submit the required result", Summary: "missing submit_result",
	}
	fullPayload, err := json.Marshal(map[string]interface{}{
		"id": "new", "status": string(TaskProtocolIncomplete), "output": "partial output",
		"failure_event": full,
	})
	if err != nil {
		t.Fatal(err)
	}
	sparsePayload := []byte(`{"id":"new","status":"protocol_incomplete","failure_event":{"summary":"updated protocol summary"}}`)
	legacyPayload := []byte(`{"id":"legacy","desc":"legacy protocol task","status":"protocol_incomplete","output":"legacy output"}`)
	reduced := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: "new", Payload: []byte(`{"id":"new","desc":"new protocol task","status":"pending"}`)},
		{Type: "task_protocol_incomplete", TaskID: "new", Payload: fullPayload},
		{Type: "task_protocol_incomplete", TaskID: "new", Payload: sparsePayload},
		{Type: "task_protocol_incomplete", TaskID: "legacy", Payload: legacyPayload},
	})
	if len(reduced) != 2 {
		t.Fatalf("protocol replay task count = %d, want 2", len(reduced))
	}
	byID := map[string]*TodoItem{}
	for _, item := range reduced {
		byID[item.ID] = item
	}
	if byID["legacy"].Status != TaskProtocolIncomplete || byID["legacy"].Desc != "legacy protocol task" {
		t.Fatalf("legacy protocol replay = %#v", byID["legacy"])
	}
	got := byID["new"]
	if got.Status != TaskProtocolIncomplete || got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureProtocol || got.FailureEvent.Command != "submit_result" || got.FailureEvent.Summary != "updated protocol summary" {
		t.Fatalf("new/sparse protocol replay = %#v", got)
	}
}

func TestWP13FailureEventIsSelfContainedAndBounded(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-wp13", "session-wp13")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "wp13", Shell: "bash"},
		},
		projectDir:             workspace,
		eventStore:             es,
		taskTracker:            NewTaskTracker(),
		emittedTaskTransitions: make(map[string]bool),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:  "worker",
		Desc:   strings.Repeat("do not persist this prompt ", 100),
		Verify: "test -f result.txt",
	}})[0]
	item.VerifyResult = &VerificationResult{
		Command:  "test -f result.txt",
		WorkDir:  workspace,
		ExitCode: 127,
		Stdout:   "",
		Stderr:   "tool: not found",
	}

	detail := c.FailureDetail(errors.New("deliverable verification failed: tool not found"), "verification")
	c.PersistFailureWithDisposition("worker", item.Desc, item.ID, detail, ReplanRequired)
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var failedPayload map[string]interface{}
	for _, event := range events {
		if event.Type != "task_failed" {
			continue
		}
		if err := json.Unmarshal(event.Payload, &failedPayload); err != nil {
			t.Fatal(err)
		}
		break
	}
	if failedPayload == nil {
		t.Fatal("expected task_failed event")
	}
	if _, ok := failedPayload["desc"]; ok {
		t.Fatal("task_failed payload must not embed the full task description")
	}
	if failedPayload["task_id"] != item.ID || failedPayload["failure_class"] != string(FailureVerify) {
		t.Fatalf("identity/class payload = %#v", failedPayload)
	}
	if failedPayload["phase"] != "verification" || failedPayload["retry_disposition"] != string(ReplanRequired) {
		t.Fatalf("phase/disposition payload = %#v", failedPayload)
	}
	if failedPayload["command"] != item.VerifyResult.Command || failedPayload["work_dir"] != workspace || failedPayload["shell"] != "bash" {
		t.Fatalf("command context payload = %#v", failedPayload)
	}
	if failedPayload["exit_code"] != float64(127) || failedPayload["stderr"] != "tool: not found" {
		t.Fatalf("verification evidence payload = %#v", failedPayload)
	}
	if failedPayload["fingerprint"] == "" || failedPayload["hint"] == "" {
		t.Fatalf("diagnostic payload missing fingerprint/hint: %#v", failedPayload)
	}
	if summary, ok := failedPayload["summary"].(string); !ok || len(summary) > 500 || strings.Contains(summary, "do not persist this prompt") {
		t.Fatalf("summary is not bounded/task-id based: %#v", failedPayload["summary"])
	}
	if _, ok := failedPayload["failure_event"]; !ok {
		t.Fatal("task_failed payload missing durable failure_event object")
	}
}

func TestWP13StructuredEnvironmentClassSurvivesPersistence(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-wp13-env", "session-wp13-env")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "wp13-env", Shell: "sh"},
		},
		projectDir:             workspace,
		eventStore:             es,
		taskTracker:            NewTaskTracker(),
		emittedTaskTransitions: make(map[string]bool),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify missing tool"}})[0]
	item.Verify = "tool status"
	item.VerifyResult = &VerificationResult{
		Command:  "tool status",
		WorkDir:  workspace,
		ExitCode: 127,
		Stderr:   "tool: command not found",
	}
	detail := "deliverable verification failed: exit code 127"
	if strings.Contains(c.FailureDetail(errors.New(detail), "verification"), "failure_class=") {
		t.Fatal("FailureDetail must not inject a text-derived failure_class tag")
	}
	structuredInput := FailureClassificationInput{
		Err:             errors.New(detail),
		ExitCode:        item.VerifyResult.ExitCode,
		ExitCodeSource:  ExitCodeSourceVerify,
		ResolveFindings: environmentFindingsFromVerifyResult(item.VerifyResult),
	}
	currentClass := ClassifyTaskFailureStructured(structuredInput)
	if currentClass != FailureEnvironment {
		t.Fatalf("structured verifier classification = %q, want %q", currentClass, FailureEnvironment)
	}
	disposition, _ := DecideRecovery(RecoveryDecisionInput{
		FailureClass:     currentClass,
		RecoveryPolicy:   RecoveryRetry,
		Attempt:          1,
		MaxRetries:       3,
		EvidenceComplete: true,
		Replayable:       true,
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
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "task_failed" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["failure_class"] != string(FailureEnvironment) || payload["retry_disposition"] != string(ReplanRequired) {
			t.Fatalf("event class/disposition = %#v, want environment/replan_required", payload)
		}
		return
	}
	t.Fatal("expected task_failed event")
}

func TestWP13FailureEventReducerSupportsLegacyAndSparseNewPayloads(t *testing.T) {
	fullEvent := FailureEventPayload{
		TaskID:           "new",
		Phase:            "execution",
		FailureClass:     FailureExecution,
		RetryDisposition: RetryWorker,
		Command:          "go test ./...",
		WorkDir:          "/workspace",
		Shell:            "sh",
		Stdout:           "partial output",
		Stderr:           "",
		Fingerprint:      "fp-1",
		Hint:             "fix the failing test",
		Summary:          "first failure",
	}
	fullPayload, err := json.Marshal(map[string]interface{}{
		"id":            "new",
		"task_id":       "new",
		"status":        "error",
		"summary":       "first failure",
		"failure_event": fullEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	sparsePayload := []byte(`{"id":"new","task_id":"new","status":"error","summary":"updated failure","failure_event":{"failure_class":"environment","summary":"updated failure"}}`)
	legacyPayload := []byte(`{"id":"legacy","desc":"legacy task description","status":"error","output":"legacy output"}`)
	createdPayload := []byte(`{"id":"new","desc":"new task description","status":"pending"}`)

	reduced := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: "new", Payload: createdPayload},
		{Type: "task_failed", TaskID: "new", Payload: fullPayload},
		{Type: "task_failed", TaskID: "new", Payload: sparsePayload},
		{Type: "task_failed", TaskID: "legacy", Payload: legacyPayload},
	})
	if len(reduced) != 2 {
		t.Fatalf("reduced tasks = %d, want 2", len(reduced))
	}
	byID := make(map[string]*TodoItem, len(reduced))
	for _, item := range reduced {
		byID[item.ID] = item
	}
	if byID["legacy"].Desc != "legacy task description" {
		t.Fatalf("legacy description = %q", byID["legacy"].Desc)
	}
	got := byID["new"]
	if got.Desc != "new task description" || got.FailureEvent == nil {
		t.Fatalf("new task replay lost identity/event: %#v", got)
	}
	if got.FailureEvent.FailureClass != FailureEnvironment || got.FailureEvent.Summary != "updated failure" {
		t.Fatalf("sparse fields did not update: %#v", got.FailureEvent)
	}
	if got.FailureEvent.Command != "go test ./..." || got.FailureEvent.Stdout != "partial output" || got.FailureEvent.Fingerprint != "fp-1" {
		t.Fatalf("sparse replay erased omitted evidence: %#v", got.FailureEvent)
	}
}

func TestWP13CheckpointTaskEventRetriesAfterAppendFailure(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-wp13-retry", "session-wp13-retry")
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:                &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp13-retry"}},
		taskTracker:            NewTaskTracker(),
		eventStore:             es,
		emittedTaskTransitions: make(map[string]bool),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
	item.Status = TaskError
	item.Detail = "execution failed"
	item.FailureEvent = &FailureEventPayload{
		TaskID: item.ID, Phase: "execution", FailureClass: FailureExecution,
		RetryDisposition: RetryNone, Summary: item.Detail,
	}

	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
	if c.DualWriteFailures() == 0 {
		t.Fatal("expected the closed event store to record a dual-write failure")
	}

	reopened, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	c.eventStore = reopened
	c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())

	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "task_failed" || event.TaskID != item.ID {
			continue
		}
		if len(event.Payload) == 0 {
			t.Fatal("retry emitted an empty task_failed payload")
		}
		return
	}
	t.Fatal("checkpoint task_failed event was not retried after append failure")
}
