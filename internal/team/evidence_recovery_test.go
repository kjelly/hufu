package team

import (
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"
)

func TestEvidenceRecoveryKeepsCanonicalExecutionOrigin(t *testing.T) {
	for _, mode := range []string{"accepted", "unselected", "missing_source", "missing_input_source", "changed_result", "changed_verification", "changed_receipt", "stale_inputs", "wrong_branch"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}},
				[]RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}}, "request-run", "request", c.session.Config.Name)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := mustArtifactStore(t, c.session.Workspace)
			put, err := artifacts.Put(t.Context(), PutArtifactRequest{Kind: "task_transcript", Path: "source.jsonl", Content: []byte("original observations"), RunID: "producer-run", TaskID: "1", Attempt: 1, Agent: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			before, err := artifacts.Get(t.Context(), put.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt := ExecutionReceipt{RunID: "producer-run", TaskID: "1", Attempt: 1, ModelExecutionID: "original-execution", ProducerID: "worker", TranscriptRef: put.ID,
				StartedAt: time.Now(), ExitCode: new(0), RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
			item := &TodoItem{ID: "1", Agent: "worker", Desc: "accepted source", Status: TaskDone, VerifyResult: &VerificationResult{ExitCode: 0, Duration: time.Second, EvaluatedAt: time.Now()},
				TypedResult:        &TaskResult{Status: TaskResultStatusSuccess, Summary: "accepted facts", Source: "submitted", Facts: map[string]any{"count": 3}},
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipt: &receipt, ExecutionReceipts: []ExecutionReceipt{receipt}}
			sd := NewSession()
			sd.Tasks, sd.RunInputSnapshots, sd.ActiveRunInputSnapshotID = []*TodoItem{item}, []RunInputSnapshot{*snapshot}, snapshot.ID
			c.SetSessionData(sd)
			if mode != "unselected" {
				if err := c.selectRecoveryRunInputSnapshot(snapshot); err != nil {
					t.Fatal(err)
				}
			}
			store, err := NewEventStore(c.session.Workspace, c.executionRunID, "recovery")
			if err != nil {
				t.Fatal(err)
			}
			c.eventStore = store
			store.SetBranchID("main")
			c.SetEventJournal(eventStoreJournal{store: store})
			payload, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "missing_input_source" {
				if err := store.Append(RunEvent{Type: string(EventRunInputsResolved), RunID: snapshot.RunID, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "missing_source" {
				c.saveCheckpoint()
			}
			switch mode {
			case "changed_result":
				c.taskTracker.TodoList().mu.Lock()
				c.taskTracker.TodoList().items[0].TypedResult.Summary = "altered facts"
				c.taskTracker.TodoList().mu.Unlock()
			case "changed_verification":
				c.taskTracker.TodoList().mu.Lock()
				c.taskTracker.TodoList().items[0].VerifyResult = &VerificationResult{ExitCode: 1}
				c.taskTracker.TodoList().mu.Unlock()
			case "changed_receipt":
				c.taskTracker.TodoList().mu.Lock()
				c.taskTracker.TodoList().items[0].ExecutionReceipt.ProducerID = "imposter"
				c.taskTracker.TodoList().items[0].ExecutionReceipts[0].ProducerID = "imposter"
				c.taskTracker.TodoList().mu.Unlock()
			case "stale_inputs":
				c.taskTracker.TodoList().mu.Lock()
				c.taskTracker.TodoList().items[0].RunInputSnapshotHash = "different"
				c.taskTracker.TodoList().mu.Unlock()
			case "wrong_branch":
				tree := NewSessionTree()
				if _, err := tree.CreateRootBranch("other"); err != nil {
					t.Fatal(err)
				}
				if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
					t.Fatal(err)
				}
				store.SetBranchID("other")
			}
			manifest, err := c.buildEvidenceManifest(t.Context(), mode != "unselected")
			if mode != "accepted" {
				if err == nil && (manifest == nil || manifest.Status != "failed") {
					t.Fatal("unauthorized recovery evidence was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			binding, ok := manifest.VerifiedTaskBinding("1")
			if !ok || manifest.Status != "accepted" || binding.RunID != receipt.RunID || binding.TranscriptRef != receipt.TranscriptRef || manifest.RecoverySnapshotHash != snapshot.SnapshotHash {
				t.Fatal("recovery lost its execution origin or frozen inputs")
			}
			if err := manifest.Verify(t.Context(), artifacts); err != nil {
				t.Fatal(err)
			}
			after, err := artifacts.Get(t.Context(), put.ID)
			if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(receipt, *todoByID(t, c, "1").ExecutionReceipt) {
				t.Fatal("recovery rewrote the original receipt or immutable artifact")
			}
			terminal := RunFinishedEventPayload{Outcome: RunOutcomeCompleted, RunInputs: snapshot, RunInputOriginRunID: snapshot.RunID, EvidenceManifest: manifest}
			raw, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{RunID: c.executionRunID, Type: "run_finished", Payload: raw}
			if err := validateRunFinishedEventPayload(event); err != nil {
				t.Fatal(err)
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToSessionData(append(events, event))
			if !reflect.DeepEqual(replayed.RunResult.EvidenceManifest, manifest) {
				t.Fatal("terminal replay changed historical evidence provenance")
			}
			manifest.RecoverySnapshotHash = "wrong"
			if err := manifest.Seal(); err != nil {
				t.Fatal(err)
			}
			if ValidateEvidenceRecoveryInputs(manifest, snapshot) == nil || manifest.Verify(t.Context(), artifacts) == nil {
				t.Fatal("recovery accepted modified frozen input binding")
			}
		})
	}
}
