package team

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resumeEvidenceFixture(t *testing.T, includeInputEvent bool) (*Coordinator, *RunInputSnapshot) {
	t.Helper()
	c, id, task := toolEvidenceRepairFixture(t)
	audit := todoByID(t, c, id)
	audit.ID = "6"
	audit.Goal = audit.Desc
	audit.EvidenceFrom = []string{"4", "5"}
	c.taskTracker.TodoList().Restore([]*TodoItem{audit})
	task = taskDefFromTodoItem(audit)
	audit = interruptedToolEvidenceFixture(t, c, audit.ID, task, 2)
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}},
		[]RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}}, c.executionRunID, "request", c.session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	audit.Status = TaskBlocked
	audit.RunInputSnapshotID, audit.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	audit.FailureEvent = &FailureEventPayload{TaskID: audit.ID, FailureClass: FailureProtocol, RetryDisposition: NeedsHuman}
	audit.ExecutionReceipt.RunInputSnapshotID, audit.ExecutionReceipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	audit.ExecutionReceipt.RepairProvenance = &RepairProvenance{Attempted: true, RepairAttempts: 2, FailureReason: RepairFailureNoToolCall,
		History: []RepairAttemptProvenance{{Attempt: 1, FailureReason: RepairFailureNoToolCall}, {Attempt: 2, FailureReason: RepairFailureNoToolCall}}}
	audit.ExecutionReceipts = []ExecutionReceipt{cloneExecutionReceipt(audit.ExecutionReceipt)}
	items := []*TodoItem{}
	for _, sourceID := range audit.EvidenceFrom {
		transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, sourceID, c.executionRunID, 1, "worker")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transcript.Close() })
		if err := transcript.RecordToolCall("source-"+sourceID, "view", `{"file_path":"document-a"}`); err != nil {
			t.Fatal(err)
		}
		if err := transcript.RecordToolResult("source-"+sourceID, "view", `{"text":"the exact words in a document"}`, false); err != nil {
			t.Fatal(err)
		}
		ref, err := sealVerbatimTaskTranscript(transcript)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := validateStructuredResultPayload(c.session.ResultContracts[audit.ResultContract.ID], *audit.ResultContract, []byte(evidenceTestPayload), transcript.evidenceRecords())
		if err != nil {
			t.Fatal(err)
		}
		receipt := &ExecutionReceipt{RunID: c.executionRunID, TaskID: sourceID, ProducerID: "worker", Attempt: 1, ExitCode: new(0), TranscriptRef: ref.ID,
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ResultContractID: audit.ResultContract.ID,
			ResultContractSHA256: audit.ResultContract.SchemaSHA256, ResultPayloadSHA256: payload.SHA256, ResultValidation: ResultValidationValid}
		items = append(items, &TodoItem{ID: sourceID, Agent: "worker", Status: TaskDone, ResultContract: audit.ResultContract.clone(),
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipt: receipt, ExecutionReceipts: []ExecutionReceipt{*receipt},
			TypedResult: &TaskResult{TaskID: sourceID, Agent: "worker", Source: "submitted", Status: TaskResultStatusSuccess, Summary: "accepted original facts", StructuredPayload: payload}})
	}
	items = append(items, audit)
	session := NewSession()
	session.Tasks, session.RunInputSnapshots, session.ActiveRunInputSnapshotID = items, []RunInputSnapshot{*snapshot}, snapshot.ID
	c.SetSessionData(session)
	c.executionRunID = "operator-repair-run"
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "resume")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	store.SetBranchID("main")
	c.SetEventJournal(eventStoreJournal{store: store})
	if err := SaveSessionTree(c.session.Workspace, NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if includeInputEvent {
		if err := store.Append(RunEvent{Type: string(EventRunInputsResolved), RunID: snapshot.RunID, Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.selectRecoveryRunInputSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	c.saveCheckpoint()
	return c, snapshot
}

func TestOperatorResultRepairReusesCompletedSourcesAndOriginalObservations(t *testing.T) {
	c, _ := resumeEvidenceFixture(t, true)
	worker := scriptWorker(scriptedTurn{err: errors.New("worker must not be replayed")})
	c.workerAgentOverride = worker
	repair := scriptWorker(submitEvidenceRepairTurn(t, c, "6"))
	c.repairAgentOverride = repair
	before := c.taskTracker.TodoList().Items()
	original := readStoredArtifact(t, c.session.Workspace, before[2].ExecutionReceipt.TranscriptRef)
	ctx := WithResultOnlyRepair(WithSessionResume(withTestProtocolRepairInvocationContext(t.Context())), "6")
	if err := c.validateResumeSources(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	granted := todoByID(t, c, "6")
	if taskProtocolRepairAttemptLimit(granted) != 4 || len(granted.ExecutionReceipt.RepairProvenance.History) != 2 {
		t.Fatal("operator authorization erased history or failed to bound additional turns")
	}
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(granted), granted); err != nil {
		t.Fatal(err)
	}
	if worker.Calls() != 0 || repair.Calls() != 1 {
		t.Fatalf("worker calls=%d repair calls=%d", worker.Calls(), repair.Calls())
	}
	for _, want := range []string{"the exact words in a document", before[0].TypedResult.StructuredPayload.SHA256, before[1].TypedResult.StructuredPayload.SHA256} {
		if !strings.Contains(repair.Prompt(t, 1), want) {
			t.Fatalf("repair lost original accepted evidence %q", want)
		}
	}
	for i, id := range []string{"4", "5"} {
		if !sameCanonicalEvidence(before[i], todoByID(t, c, id)) {
			t.Fatalf("completed source %s changed during repair", id)
		}
	}
	after := todoByID(t, c, "6")
	if after.Status != TaskDone || after.ExecutionReceipt.RunID != before[2].ExecutionReceipt.RunID ||
		after.ExecutionReceipt.TranscriptRef != before[2].ExecutionReceipt.TranscriptRef ||
		!bytes.Equal(original, readStoredArtifact(t, c.session.Workspace, after.ExecutionReceipt.TranscriptRef)) {
		t.Fatal("result-only repair changed the original execution identity or transcript")
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil || len(replayed) != 3 || !sameCanonicalEvidence(after.ExecutionReceipt.RepairProvenance, replayed[2].ExecutionReceipt.RepairProvenance) {
		t.Fatalf("canonical replay lost operator grant or prior repairs: %v", err)
	}
}

func TestResumeEvidencePreflightRejectsChangedSourcesBeforeRepair(t *testing.T) {
	for _, mode := range []string{"missing_evidence", "changed_payload", "changed_inputs", "missing_inputs_event", "wrong_team", "changed_contract", "wrong_task"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := resumeEvidenceFixture(t, mode != "missing_inputs_event")
			items := c.taskTracker.TodoList().Items()
			switch mode {
			case "missing_evidence":
				store := mustArtifactStore(t, c.session.Workspace)
				if err := os.Remove(filepath.Join(store.root, "data", items[0].ExecutionReceipt.TranscriptRef)); err != nil {
					t.Fatal(err)
				}
			case "changed_payload":
				items[0].TypedResult.StructuredPayload.SHA256 = strings.Repeat("0", 64)
			case "changed_inputs":
				items[0].RunInputSnapshotHash = "changed"
			case "changed_contract":
				c.session.ResultContracts[items[0].ResultContract.ID].SchemaSHA256 = "changed"
			case "wrong_team":
				c.session.Config.Name = "other"
			}
			c.taskTracker.TodoList().Restore(items)
			ctx := WithResultOnlyRepair(WithSessionResume(t.Context()), "6")
			if mode == "wrong_task" {
				ctx = WithResultOnlyRepair(ctx, "missing")
			}
			before, err := c.eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.validateResumeSources(ctx); err == nil || !strings.Contains(err.Error(), "available recovery branches") {
				t.Fatalf("invalid recovery accepted without sources diagnostic: %v", err)
			}
			after, err := c.eventStore.ReadEvents()
			if err != nil || len(before) != len(after) {
				t.Fatal("preflight refusal dispatched work or changed canonical evidence")
			}
		})
	}
}

func TestOperatorResultRepairExhaustionDoesNotGrantOrReplayAutomatically(t *testing.T) {
	c, _ := resumeEvidenceFixture(t, true)
	worker := scriptWorker(scriptedTurn{err: errors.New("worker replay forbidden")})
	c.workerAgentOverride = worker
	repair := scriptWorker(textTurn("report ready"))
	c.repairAgentOverride = repair
	ctx := WithResultOnlyRepair(withTestProtocolRepairInvocationContext(t.Context()), "6")
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	item := todoByID(t, c, "6")
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(item), item); err == nil {
		t.Fatal("missing submissions accepted")
	}
	item = todoByID(t, c, "6")
	if repair.Calls() != 2 || worker.Calls() != 0 || item.Status != TaskBlocked || len(item.ExecutionReceipt.RepairProvenance.History) != 4 {
		t.Fatal("operator cycle was not bounded or lost old repair history")
	}
	if err := c.authorizeOperatorResultRepair(ctx); err == nil {
		t.Fatal("same invocation granted itself more submission turns")
	}
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(item), item); err == nil || repair.Calls() != 2 || worker.Calls() != 0 {
		t.Fatal("exhausted repair replayed a model or worker")
	}
	if n, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context())); err != nil || n != 0 || repair.Calls() != 2 {
		t.Fatalf("ordinary resume silently granted another cycle: n=%d err=%v", n, err)
	}
}
