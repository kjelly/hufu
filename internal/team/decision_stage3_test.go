package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func stage3PartialDecision(t *testing.T) (*FileArtifactStore, *memoryJournal, DecisionRequest, ArtifactRef) {
	return stage3PartialDecisionWithPolicy(t, enginePolicy(3))
}

func stage3PartialDecisionWithPolicy(t *testing.T, policy DecisionPolicy) (*FileArtifactStore, *memoryJournal, DecisionRequest, ArtifactRef) {
	return stage3PartialDecisionWithRunID(t, policy, "run-1")
}

func stage3PartialDecisionWithRunID(t *testing.T, policy DecisionPolicy, runID string) (*FileArtifactStore, *memoryJournal, DecisionRequest, ArtifactRef) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		if judgeID == "judge-3" {
			return DecisionOpinion{}, fmt.Errorf("simulated interruption")
		}
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	req := engineRequest(policy)
	req.RunID = runID
	req.DecisionID = "decision-stage3"
	legacyTask := TaskDef{ID: req.TaskID, Agent: "worker", Goal: req.Question, DecisionOptions: append([]DecisionOption(nil), req.Options...)}
	// The envelope is admitted against the same persisted Todo occurrence that
	// recovery will later validate. A TaskDef-only digest has no runtime
	// occurrence identity and cannot represent the durable projection.
	persistedOccurrence := todoItemFromSpec(todoSpecForTestTask(legacyTask), req.TaskID)
	digest, err := decisionTaskInputDigest(persistedOccurrence)
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionInputDigest = digest
	engine := newTestEngineWithStages(journal, runner, nil, DecisionServices{Store: store})
	if _, err := engine.Run(context.Background(), req); err == nil {
		t.Fatal("partial decision unexpectedly finalized")
	}
	state, err := projectDecision(context.Background(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.EnvelopeRef.ID == "" {
		t.Fatal("partial decision did not anchor its run envelope")
	}
	return store, journal, req, state.EnvelopeRef
}

func TestDecisionResumeUsesAnchoredOccurrenceAcrossRecoveryReceipts(t *testing.T) {
	policy := enginePolicy(3)
	policy.Discipline = DisciplinePolicy{
		Stop:   StopPolicy{CheckpointEvery: 7},
		Commit: CommitGatePolicy{RequireVerification: true},
		Replan: ReplanPolicy{OnCriticalAssumptionContradicted: agent.ReplanStop},
	}
	store, journal, req, _ := stage3PartialDecisionWithRunID(t, policy, "run-A")

	// Finish the durable judge quorum without giving the resumed coordinator a
	// sidecar. This leaves the run unfinished while ensuring resume reaches the
	// actual runtime arming boundary.
	req.Attempt = 1
	state, err := projectDecision(context.Background(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	opinion := scoredOpinion(3, 9, "wait", 0.3)
	opinion.ID, opinion.JudgeID, opinion.EvidenceHash, opinion.Round, opinion.Valid = "opinion-3", "judge-3", state.Packet.Hash, 1, true
	event := decisionEventFor(req, "opinion", req.DecisionID+"-evidence", "judge-3", "1")
	event.EvidenceHash, event.JudgeID, event.Round, event.Opinion = state.Packet.Hash, "judge-3", 1, &opinion
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionOpinionSubmitted, event); err != nil {
		t.Fatal(err)
	}
	state, err = projectDecision(context.Background(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}

	tracker := NewTaskTracker()
	legacyTask := TaskDef{ID: req.TaskID, Agent: "worker", Goal: req.Question, DecisionOptions: append([]DecisionOption(nil), req.Options...)}
	creation := &Coordinator{
		eventJournal: journal,
		taskTracker:  tracker,
		session: &TeamSession{
			Workspace: filepath.Dir(filepath.Dir(store.root)),
			Config:    agent.TeamConfig{WorkspaceDir: filepath.Dir(filepath.Dir(store.root))},
		},
	}
	item := todoItemFromSpec(todoSpecForTestTask(legacyTask), req.TaskID)
	payload, err := json.Marshal(creation.taskTransitionPayloadWithCoordinator(item))
	if err != nil {
		t.Fatalf("marshal task_created projection: %v", err)
	}
	if _, err := journal.Append(context.Background(), RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}); err != nil {
		t.Fatalf("append task_created projection: %v", err)
	}
	tracker.TodoList().AddReserved([]*TodoItem{item})
	item.Status = TaskInProgress
	for _, runID := range []string{"run-A", "run-B"} {
		if err := tracker.TodoList().SetExecutionReceipt(item.ID, &ExecutionReceipt{
			RunID: runID, TaskID: item.ID, Attempt: 1, ProducerID: item.Agent,
		}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := &Coordinator{
		executionRunID: "run-B",
		eventJournal:   journal,
		taskTracker:    tracker,
		session: &TeamSession{
			Workspace: filepath.Dir(filepath.Dir(store.root)),
			Config:    agent.TeamConfig{Decision: DecisionConfig{}, WorkspaceDir: filepath.Dir(filepath.Dir(store.root))},
		},
	}

	// The live profile/configuration changed after the crash, but neither is
	// consulted before the checkpoint-owned occurrence is resumed. The task
	// input itself must remain the exact task_created projection.
	coordinator.session.Config.Decision.DefaultProfile = "changed-current-profile"
	changed := taskDefFromTodoItem(item)
	changed.Goal = "changed current task"
	if _, err := coordinator.prepareTaskDecision(context.Background(), changed, item.ID); err == nil || !strings.Contains(err.Error(), "immutable goal") {
		t.Fatalf("changed task input was not rejected by immutable occurrence owner: %v", err)
	}
	disarm, err := coordinator.prepareTaskDecision(context.Background(), taskDefFromTodoItem(item), item.ID)
	if err != nil {
		t.Fatalf("prepareTaskDecision resumed under changed profile: %v", err)
	}
	defer disarm()
	discipline := coordinator.disciplineFor(item.ID)
	if discipline == nil {
		t.Fatal("resume did not arm execution discipline")
	}
	if discipline.stop.CheckpointEvery != 7 || !discipline.commit.RequireVerification || discipline.replan.OnCriticalAssumptionContradicted != agent.ReplanStop {
		t.Fatalf("discipline policy = %#v, want the envelope policy %#v", discipline, policy.Discipline)
	}
}

func TestDecisionOccurrenceLookupFindsAnchorBeforeAnyWorkerReceipt(t *testing.T) {
	store, journal, req, _ := stage3PartialDecisionWithRunID(t, enginePolicy(3), "run-A")
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume decision"}})[0]
	item.ID = req.TaskID
	item.Status = TaskInProgress
	coordinator := &Coordinator{
		executionRunID: "run-B",
		eventJournal:   journal,
		taskTracker:    tracker,
		session: &TeamSession{
			Workspace: filepath.Dir(filepath.Dir(store.root)),
			Config:    agent.TeamConfig{WorkspaceDir: filepath.Dir(filepath.Dir(store.root))},
		},
	}

	got, err := coordinator.decisionForTaskOccurrence(context.Background(), item.ID, 1)
	if err != nil {
		t.Fatalf("decision occurrence lookup: %v", err)
	}
	if got != req.DecisionID {
		t.Fatalf("decision occurrence = %q, want original anchored %q", got, req.DecisionID)
	}
}

func TestDecisionOccurrenceLookupFailsClosedForCorruptBranchTree(t *testing.T) {
	store, journal, req, _ := stage3PartialDecisionWithRunID(t, enginePolicy(3), "run-A")
	workspace := filepath.Dir(filepath.Dir(store.root))
	if err := os.WriteFile(filepath.Join(workspace, sessionTreeFile), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume decision"}})[0]
	item.ID = req.TaskID
	item.Status = TaskInProgress
	coordinator := &Coordinator{
		executionRunID: "run-B", eventJournal: journal, taskTracker: tracker,
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkspaceDir: workspace}},
	}
	if _, err := coordinator.decisionForTaskOccurrence(context.Background(), item.ID, 1); err == nil {
		t.Fatal("occurrence lookup accepted a corrupt session tree")
	} else if !strings.Contains(err.Error(), "session tree") {
		t.Fatalf("lookup error = %v, want session-tree diagnostic", err)
	}
}

func TestDecisionOccurrenceLookupFailsClosedForMissingActiveBranch(t *testing.T) {
	store, journal, req, _ := stage3PartialDecisionWithRunID(t, enginePolicy(3), "run-A")
	workspace := filepath.Dir(filepath.Dir(store.root))
	if err := os.WriteFile(filepath.Join(workspace, sessionTreeFile), []byte(`{"active_branch":"missing","branches":{"main":{"id":"main","name":"main"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume decision"}})[0]
	item.ID = req.TaskID
	item.Status = TaskInProgress
	coordinator := &Coordinator{
		executionRunID: "run-B", eventJournal: journal, taskTracker: tracker,
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkspaceDir: workspace}},
	}
	if _, err := coordinator.decisionForTaskOccurrence(context.Background(), item.ID, 1); err == nil {
		t.Fatal("occurrence lookup accepted a missing active branch")
	} else if !strings.Contains(err.Error(), "active session branch") {
		t.Fatalf("lookup error = %v, want missing-branch diagnostic", err)
	}
}

func TestDecisionOccurrenceLookupFailsClosedForCyclicActiveBranch(t *testing.T) {
	store, journal, req, _ := stage3PartialDecisionWithRunID(t, enginePolicy(3), "run-A")
	workspace := filepath.Dir(filepath.Dir(store.root))
	if err := os.WriteFile(filepath.Join(workspace, sessionTreeFile), []byte(`{"active_branch":"main","branches":{"main":{"id":"main","name":"main","parent_id":"main"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume decision"}})[0]
	item.ID = req.TaskID
	item.Status = TaskInProgress
	coordinator := &Coordinator{
		executionRunID: "run-B", eventJournal: journal, taskTracker: tracker,
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkspaceDir: workspace}},
	}
	if _, err := coordinator.decisionForTaskOccurrence(context.Background(), item.ID, 1); err == nil {
		t.Fatal("occurrence lookup accepted a cyclic active branch")
	} else if !strings.Contains(err.Error(), "lineage contains a cycle") {
		t.Fatalf("lookup error = %v, want cycle diagnostic", err)
	}
}

func TestDecisionTransitionKeysIgnoreRetryTimestamps(t *testing.T) {
	journal := &memoryJournal{}
	assumptions := []DecisionAssumption{{ID: "A1", Statement: "traffic stays flat"}}
	for _, at := range []time.Time{
		time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 5, 12, 0, 1, 0, time.UTC),
	} {
		if _, _, err := RecordAssumptionTransition(context.Background(), journal, assumptions, AssumptionTransition{
			DecisionID: "decision-transition", AssumptionID: "A1", From: AssumptionUnknown,
			To: AssumptionSupported, Source: AssumptionSourceOperator, At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := RequestReplan(context.Background(), journal, "decision-transition", CheckpointDecision{
		Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated, Detail: "traffic doubled",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RequestReplan(context.Background(), journal, "decision-transition", CheckpointDecision{
		Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated, Detail: "traffic doubled",
	}); err != nil {
		t.Fatal(err)
	}

	var keys []string
	for _, event := range journal.events {
		if event.Type == agent.EventAssumptionSupported || event.Type == agent.EventReplanRequested {
			keys = append(keys, event.IdempotencyKey)
		}
	}
	if len(keys) != 4 || keys[0] == "" || keys[0] != keys[1] || keys[2] == "" || keys[2] != keys[3] {
		t.Fatalf("retry transition keys = %v, want stable semantic keys", keys)
	}
	if strings.Contains(keys[0], "2026") || strings.Contains(keys[2], "2026") {
		t.Fatalf("transition key contains timestamp data: %v", keys)
	}
}

func TestDecisionEngineStage3FreshEngineResumeReusesTwoOpinions(t *testing.T) {
	store, journal, req, _ := stage3PartialDecision(t)
	recovered := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		if judgeID != "judge-3" {
			t.Fatalf("fresh resume dispatched already durable %s", judgeID)
		}
		return scoredOpinion(3, 9, "wait", 0.3), nil
	})
	engine := newTestEngineWithStages(journal, recovered, nil, DecisionServices{Store: store})
	record, err := engine.Resume(context.Background(), req.DecisionID)
	if err != nil {
		t.Fatalf("fresh-engine resume: %v", err)
	}
	if got := recovered.dispatched(); len(got) != 1 || got[0] != "judge-3" {
		t.Fatalf("resume dispatched %v, want only judge-3", got)
	}
	if len(record.Opinions) != 3 || journal.count(agent.EventDecisionFinalized) != 1 {
		t.Fatalf("resume record/events = opinions %d, finalized %d", len(record.Opinions), journal.count(agent.EventDecisionFinalized))
	}
}

func TestDecisionEngineStage3RepeatFinalizedFreshEngineIsNoop(t *testing.T) {
	store, journal, req, _ := stage3PartialDecision(t)
	recovered := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(3, 9, "wait", 0.3), nil
	})
	engine := newTestEngineWithStages(journal, recovered, nil, DecisionServices{Store: store})
	first, err := engine.Resume(context.Background(), req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	secondEngine := newTestEngineWithStages(journal, newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		t.Fatal("finalized resume dispatched a judge")
		return DecisionOpinion{}, nil
	}), nil, DecisionServices{Store: store})
	second, err := secondEngine.Resume(context.Background(), req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) || journal.count(agent.EventDecisionFinalized) != 1 {
		t.Fatalf("repeat finalized resume changed the final record: first=%s second=%s", firstJSON, secondJSON)
	}
}

func TestDecisionEngineStage3RejectsMissingCorruptAndMismatchedEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FileArtifactStore, *memoryJournal, ArtifactRef)
	}{
		{name: "missing", mutate: func(store *FileArtifactStore, _ *memoryJournal, ref ArtifactRef) {
			if err := os.Remove(filepath.Join(store.root, "data", ref.ID)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt", mutate: func(store *FileArtifactStore, _ *memoryJournal, ref ArtifactRef) {
			if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("corrupt"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mismatch", mutate: func(_ *FileArtifactStore, journal *memoryJournal, ref ArtifactRef) {
			journal.mu.Lock()
			defer journal.mu.Unlock()
			for i := range journal.events {
				if journal.events[i].Type != agent.EventDecisionRunEnvelopeAnchored {
					continue
				}
				journal.events[i].Payload = []byte(`{"decision_id":"decision-stage3","task_id":"task-1","envelope_ref":{"id":"` + ref.ID + `","sha256":"wrong"},"envelope_hash":"wrong"}`)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, journal, req, ref := stage3PartialDecision(t)
			tc.mutate(store, journal, ref)
			engine := newTestEngineWithStages(journal, newRecordingRunner(func(string, int) (DecisionOpinion, error) {
				t.Fatal("invalid envelope dispatched a judge")
				return DecisionOpinion{}, nil
			}), nil, DecisionServices{Store: store})
			if _, err := engine.Resume(context.Background(), req.DecisionID); err == nil {
				t.Fatal("resume accepted invalid envelope")
			} else if !strings.Contains(err.Error(), "envelope") {
				t.Fatalf("resume error = %v, want envelope diagnostic", err)
			}
		})
	}
}

func TestDecisionEngineStage3EnvelopeCommitOrdering(t *testing.T) {
	store, journal, req, ref := stage3PartialDecision(t)
	if _, err := store.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("envelope was not durable: %v", err)
	}
	types := journal.typesOf()
	anchor, opinion := -1, -1
	for i, eventType := range types {
		if eventType == agent.EventDecisionRunEnvelopeAnchored {
			anchor = i
		}
		if eventType == agent.EventDecisionOpinionSubmitted && opinion == -1 {
			opinion = i
		}
	}
	if anchor == -1 || opinion == -1 || anchor >= opinion {
		t.Fatalf("envelope anchor/opinion ordering = %d/%d (%v)", anchor, opinion, types)
	}
	if req.TaskID != "task-1" {
		t.Fatalf("test request task identity changed: %q", req.TaskID)
	}
}

func TestDecisionEngineStage3LegacyFinalizedResumeRemainsReadable(t *testing.T) {
	journal := &memoryJournal{}
	record := DecisionRecord{SchemaVersion: 1, ID: "legacy", RunID: "run-1", TaskID: "task-1", FinalOption: "wait"}
	raw, err := marshalDecisionEvent(decisionEvent{DecisionID: "legacy", Record: &record})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), RunEvent{Type: agent.EventDecisionFinalized, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	engine := NewDecisionEngine(DecisionServices{Journal: journal})
	got, err := engine.Resume(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("legacy resume: %v", err)
	}
	if got.ID != record.ID || got.FinalOption != record.FinalOption {
		t.Fatalf("legacy record = %#v", got)
	}
}

func TestDecisionEngineStage4FinalizationSchemaDiscriminator(t *testing.T) {
	for _, recordSchema := range []int{1, DecisionRecordSchemaVersion} {
		for _, eventSchema := range []int{eventStoreLegacySchemaVersion, eventStoreSchemaVersion} {
			t.Run(fmt.Sprintf("record-v%d-event-v%d", recordSchema, eventSchema), func(t *testing.T) {
				journal := &memoryJournal{}
				record := DecisionRecord{
					SchemaVersion: recordSchema,
					ID:            "schema-discriminator",
					RunID:         "run-1",
					TaskID:        "task-1",
					FinalOption:   "wait",
				}
				raw, err := marshalDecisionEvent(decisionEvent{
					DecisionID: record.ID,
					RunID:      record.RunID,
					TaskID:     record.TaskID,
					Attempt:    1,
					Record:     &record,
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := journal.Append(context.Background(), RunEvent{
					SchemaVersion: eventSchema,
					RunID:         record.RunID,
					TaskID:        record.TaskID,
					Attempt:       1,
					Actor:         decisionActor,
					Type:          agent.EventDecisionFinalized,
					Payload:       raw,
				}); err != nil {
					t.Fatal(err)
				}

				got, err := NewDecisionEngine(DecisionServices{Journal: journal}).Resume(context.Background(), record.ID)
				if recordSchema == 1 {
					if err != nil {
						t.Fatalf("schema-v1 finalized-only replay: %v", err)
					}
					if got.ID != record.ID || got.SchemaVersion != record.SchemaVersion {
						t.Fatalf("replayed legacy record = %#v, want %#v", got, record)
					}
					return
				}
				if err == nil {
					t.Fatalf("schema-v%d finalized-only replay succeeded: %#v", recordSchema, got)
				}
				if !strings.Contains(err.Error(), "no preceding finalization result") {
					t.Fatalf("schema-v%d finalized-only replay error = %v, want missing-result diagnostic", recordSchema, err)
				}
			})
		}
	}
}

func stage4FinalizedReplayFixture(t *testing.T) (*memoryJournal, *FileArtifactStore, *DecisionRecord) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	journal := &memoryJournal{}
	req := engineRequest(enginePolicy(1))
	record, err := newTestEngineWithStages(journal, spreadRunner(), nil, DecisionServices{Store: store}).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("create finalized replay fixture: %v", err)
	}
	if record.SchemaVersion != DecisionRecordSchemaVersion || record.FinalizationResultRef == nil {
		t.Fatalf("fixture record = %#v, want schema-v%d with finalization result ref", record, DecisionRecordSchemaVersion)
	}
	if journal.count(agent.EventDecisionFinalizationResult) != 1 || journal.count(agent.EventDecisionFinalized) != 1 {
		t.Fatalf("fixture finalization events = %v, want one result before one finalized event", journal.typesOf())
	}
	return journal, store, record
}

func mutateDecisionEventPayload(t *testing.T, journal *memoryJournal, eventType string, mutate func(*decisionEvent)) {
	t.Helper()
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for index := range journal.events {
		if journal.events[index].Type != eventType {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(journal.events[index].Payload, &payload); err != nil {
			t.Fatalf("decode %s fixture event: %v", eventType, err)
		}
		mutate(&payload)
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode %s fixture event: %v", eventType, err)
		}
		journal.events[index].Payload = encoded
		return
	}
	t.Fatalf("fixture is missing %s event", eventType)
}

func TestDecisionEngineStage4V2FinalizedReplayRequiresMatchingResultAndArtifact(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*decisionEvent)
		want   string
	}{
		{
			name: "result mismatch",
			mutate: func(payload *decisionEvent) {
				payload.Record.FinalOption = "wait"
			},
			want: "does not match its finalization result",
		},
		{
			name: "artifact mismatch",
			mutate: func(payload *decisionEvent) {
				payload.Record.FinalizationResultRef = &ArtifactRef{ID: "different-result", SHA256: "different-result"}
			},
			want: "does not match its finalization result",
		},
		{
			name: "missing record artifact reference",
			mutate: func(payload *decisionEvent) {
				payload.Record.FinalizationResultRef = nil
			},
			want: "does not match its finalization result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal, store, record := stage4FinalizedReplayFixture(t)
			mutateDecisionEventPayload(t, journal, agent.EventDecisionFinalized, tt.mutate)
			if _, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID); err == nil {
				t.Fatal("replay accepted mismatched finalization result/artifact")
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("replay error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecisionEngineStage4V2FinalizedReplayWithMatchingResultAndArtifact(t *testing.T) {
	journal, store, record := stage4FinalizedReplayFixture(t)
	got, err := NewDecisionEngine(DecisionServices{Journal: journal, Store: store}).Resume(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("matching v2 finalization replay: %v", err)
	}
	if got.ID != record.ID || got.SchemaVersion != DecisionRecordSchemaVersion || got.FinalizationResultRef == nil {
		t.Fatalf("matching v2 replay = %#v, want original v%d record with result ref", got, DecisionRecordSchemaVersion)
	}
}

func TestDecisionOccurrenceLookupUsesCheckpointedRunAfterStartupReroute(t *testing.T) {
	journal := &memoryJournal{}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume decision"}})[0]
	item.Status = TaskInProgress
	if err := tracker.TodoList().SetExecutionReceipt(item.ID, &ExecutionReceipt{
		RunID: "run-original", TaskID: item.ID, Attempt: 1, ProducerID: item.Agent,
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(decisionEvent{
		DecisionID: "decision-original", RunID: "run-original", TaskID: item.ID, Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), RunEvent{
		Type: agent.EventDecisionStarted, RunID: "run-original", TaskID: item.ID, Attempt: 1,
		IdempotencyKey: "decision:decision-original:started", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		executionRunID: "run-new-startup-invocation",
		taskTracker:    tracker,
		eventJournal:   journal,
	}
	got, err := coordinator.decisionForTaskOccurrence(context.Background(), item.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != "decision-original" {
		t.Fatalf("decision occurrence = %q, want checkpointed decision-original", got)
	}
}

func TestDecisionPostEnvelopeEventReceivesAnchoredProvenance(t *testing.T) {
	store, journal, req, _ := stage3PartialDecision(t)
	state, err := projectDecision(context.Background(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.EnvelopeRef.ID == "" {
		t.Fatal("missing envelope anchor")
	}
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionInvalidated, decisionEvent{
		DecisionID: req.DecisionID, Reason: "test provenance",
	}); err != nil {
		t.Fatal(err)
	}
	events := journal.events
	last := events[len(events)-1]
	var payload decisionEvent
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if last.RunID != req.RunID || last.TaskID != req.TaskID || last.Attempt != 1 ||
		payload.RunID != req.RunID || payload.TaskID != req.TaskID || payload.Attempt != 1 ||
		last.IdempotencyKey == "" {
		t.Fatalf("post-envelope provenance = outer %#v payload %#v", last, payload)
	}
	if _, err := store.Resolve(context.Background(), state.EnvelopeRef); err != nil {
		t.Fatalf("anchored envelope unavailable: %v", err)
	}
}

func marshalDecisionEvent(event decisionEvent) ([]byte, error) {
	return json.Marshal(event)
}
