package team

import (
	"context"
	"reflect"
	"testing"
)

func TestResumeSettlesCompletedWorkflowPhases(t *testing.T) {
	for _, mode := range []string{"completed", "multiple_completed", "missing_contract", "pending", "protocol", "blocked", "cancelled", "failed", "verify_missing_evidence", "verify_evidence", "optional_verify"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.session = workflowTestSession(t)
			w, err := newRuntimeWorkflow(c.session)
			if err != nil {
				t.Fatal(err)
			}
			c.phaseWorkflow = w
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			sd := NewSession()
			sd.WorkflowState = PhasePrepare
			sd.Tasks = []*TodoItem{{ID: "1", Agent: "preparer", Desc: "retained preparation", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone, Output: "accepted evidence"}}
			want := PhaseAudit
			switch mode {
			case "multiple_completed":
				sd.Tasks = append(sd.Tasks,
					&TodoItem{ID: "2", Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone},
					&TodoItem{ID: "3", Agent: "executor", ContractID: "execute", Phase: PhaseExecute, Status: TaskDone})
				want = PhaseVerify
			case "missing_contract":
				sd.Tasks[0].ContractID = "other"
				want = PhasePrepare
			case "pending", "protocol", "blocked":
				status := map[string]TaskStatus{"pending": TaskPending, "protocol": TaskProtocolIncomplete, "blocked": TaskBlocked}[mode]
				sd.Tasks = append(sd.Tasks, &TodoItem{ID: "2", Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: status})
				want = PhasePrepare
			case "cancelled":
				want = PhasePrepare
			case "failed":
				sd.WorkflowState = PhaseFailed
				want = PhaseFailed
			case "verify_missing_evidence", "verify_evidence", "optional_verify":
				w.phases = []Phase{PhaseVerify}
				w.verificationRequired = true
				sd.WorkflowState = PhaseVerify
				sd.Tasks[0].Phase = PhaseVerify
				sd.Tasks[0].ContractID = "verify"
				sd.Tasks[0].Agent = "verifier"
				want = PhaseFailed
				if mode != "verify_missing_evidence" {
					sd.Tasks[0].VerifyResult = &VerificationResult{ExitCode: 0}
					want = PhaseDone
				}
				if mode == "optional_verify" {
					w.phaseOptionalContracts[PhaseVerify] = map[string]bool{"critic": true}
					want = PhaseVerify
				}
			}
			c.SetSessionData(sd)
			store, err := NewEventStore(c.session.Workspace, c.executionRunID, "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c.eventStore = store
			c.SetEventJournal(eventStoreJournal{store: store})
			c.saveCheckpoint()
			before := c.taskTracker.TodoList().Items()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			// Incomplete repair is tested at the phase settlement boundary; its
			// actual worker recovery is covered by protocol repair tests.
			if mode == "pending" || mode == "protocol" || mode == "blocked" {
				err = c.reconcileRecoveredWorkflow(ctx)
			} else {
				var count int
				count, err = c.ResumeInterruptedTasks(ctx)
				if count != 0 {
					t.Fatalf("completed work was replayed: %d", count)
				}
			}
			wantErr := mode == "cancelled" || mode == "verify_missing_evidence"
			if (err != nil) != wantErr || w.State() != want {
				t.Fatalf("phase=%s want=%s error=%v", w.State(), want, err)
			}
			if !reflect.DeepEqual(before, c.taskTracker.TodoList().Items()) {
				t.Fatal("phase settlement modified accepted tasks or evidence")
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			loaded, _, err := LoadSessionReadOnly(c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			projected := ReduceToSessionData(events)
			if loaded.WorkflowState != want || projected.WorkflowState != want || !reflect.DeepEqual(loaded.PhaseResults, projected.PhaseResults) {
				t.Fatalf("workflow checkpoint and replay disagree: stored=%s replay=%s want=%s", loaded.WorkflowState, projected.WorkflowState, want)
			}
			if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil && mode != "verify_missing_evidence" {
				t.Fatal(err)
			}
			after, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if mode != "cancelled" && len(after) != len(events) {
				t.Fatal("repeated settlement emitted duplicate events")
			}
		})
	}
}
