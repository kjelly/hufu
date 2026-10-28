package team

import (
	"encoding/json/v2"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt/catalog"
)

// Close the actual store and discard every occurrence, result latch, decision
// generator and task projection. Only configuration and persisted bytes survive.
func reopenRecoveryCoordinator(t *testing.T, previous *Coordinator) *Coordinator {
	t.Helper()
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(t)
	c.session = cloneSession(previous.session, previous.session.Workspace)
	c.projectDir = previous.projectDir
	c.sidecarModel = previous.sidecarModel
	c.maxConcurrent = previous.maxConcurrent
	c.providerManager = previous.providerManager
	c.providerBoundaryStarted = previous.providerBoundaryStarted
	if previous.modelProfileRuntime != nil {
		c.modelProfileRuntime = NewModelProfileRuntime(c.providerManager, false)
	}
	var err error
	c.decisionPrimitives, err = catalog.New(c.session.Config.DecisionPrimitives, catalog.WithSidecarGenerator((&decisionSidecarResolver{defaultModel: c.sidecarModel}).generator))
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(tasks)
	return c
}

func TestResultCollectionRecoveryRestartsAtDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_grant", "after_grant", "reserved", "response", "prepared", "accepted"} {
		t.Run(boundary, func(t *testing.T) {
			body := `{"status":"success","summary":"original limitation retained"}`
			if boundary == "reserved" {
				body = `{"status":`
			}
			c, id, original := blockedRecoveryFixture(t, body, boundary != "response" && boundary != "reserved", RepairFailureSource)
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "operator-recovery"
			if boundary != "before_grant" {
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
					t.Fatal(err)
				}
			}
			cp := currentResultCollection(c.todoItemByID(id))
			if boundary == "reserved" || boundary == "response" || boundary == "prepared" || boundary == "accepted" {
				ctx, err := c.restoreCollectionContext(WithSessionResume(t.Context()), cp)
				if err != nil {
					t.Fatal(err)
				}
				// A new reservation is allowed only after the earlier response has
				// failed validation. Exercise this using a bounded native repair grant.
				if boundary == "reserved" {
					_, err = c.reserveResultCollection(ctx, cp, "lost-response", []byte(`{"tools":[]}`))
					if err != nil {
						t.Fatal(err)
					}
				} else if boundary == "accepted" {
					if err := c.commitPreparedCollection(ctx, cp); err != nil {
						t.Fatal(err)
					}
				}
			}
			used := currentResultCollection(c.todoItemByID(id)).TurnsUsed
			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "operator-recovery"
			if boundary == "before_grant" {
				a, err := c.AssessResultCollectionRecovery(t.Context(), id)
				if err != nil || a.Disposition != RecoveryCommitLocal {
					t.Fatalf("reopened assessment=%+v err=%v", a, err)
				}
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
					t.Fatal(err)
				}
			}
			worker := scriptWorker(textTurn("must never run"))
			c.workerAgentOverride = worker
			_, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id)
			item := c.todoItemByID(id)
			if boundary == "reserved" {
				// A lost response is not refunded or treated as accepted data.
				if err == nil || item.Status == TaskDone || currentResultCollection(item).TurnsUsed < used || worker.Calls() != 0 {
					t.Fatalf("lost reservation resumed unsafely: %+v err=%v", item, err)
				}
				return
			}
			if err != nil || item.Status != TaskDone || worker.Calls() != 0 || currentResultCollection(item).TurnsUsed != used || currentResultCollection(item).Work != original.Work {
				t.Fatalf("boundary=%s status=%s calls=%d err=%v", boundary, item.Status, worker.Calls(), err)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			failures := 0
			for _, event := range events {
				if event.Type == string(EventTaskBlocked) {
					failures++
				}
			}
			if failures == 0 {
				t.Fatal("historical failure disappeared")
			}
		})
	}
}

func TestResultCollectionRecoveryIndependentClaims(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ResultCollectionRecoveryDisposition
	}{
		{"invalid summary with forged receipt", `{"status":"success","summary":42,"receipt_ids":["invented"]}`, RecoveryBlocked},
		{"invalid status with malformed receipt", `{"status":42,"summary":"report","receipt_ids":42}`, RecoveryBlocked},
		{"invalid summary only", `{"status":"success","summary":42}`, RecoveryRepairClaims},
		{"memory context unsupported", `{"status":"success","summary":"report","memory_uses":[{"context_item_id":"x"}]}`, RecoveryBlocked},
		{"assumption context unsupported", `{"status":"success","summary":"report","assumption_checks":[{"assumption_id":"x","status":"supported"}]}`, RecoveryBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, tc.body, false, RepairFailureSource)
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != tc.want {
				t.Fatalf("assessment=%+v err=%v", a, err)
			}
			if a.TaskID != id || a.BranchID != "main" || a.SourceRunID != "run-test" {
				t.Fatalf("missing assessment identity: %+v", a)
			}
		})
	}
}

// Ensure the new optional scope fields do not invalidate persisted v1 grants.
func TestResultCollectionRecoveryLegacyAssessmentFingerprint(t *testing.T) {
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"report"}`, true, RepairFailureSource)
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	a.TaskID, a.BranchID, a.SourceRunID = "", "", ""
	a.ValidatorRevision = 1
	a.Fingerprint, err = recoveryAssessmentFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var restored ResultCollectionRecoveryAssessment
	if err := json.Unmarshal(body, &restored, collectionDurationFormat); err != nil || validateRecoveryAssessment(&restored) != nil {
		t.Fatal("legacy assessment no longer validates", err)
	}
}

func TestResultCollectionDecisionRecoveryPreviewRetainsEvidenceWithoutSnapshot(t *testing.T) {
	c, id, _, calls := decisionRecoveryFixture(t)
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := NewDryRunCoordinator(DryRunCoordinatorParams{Session: cloneSession(c.session, c.session.Workspace), DefaultProviderURL: "http://127.0.0.1:1/v1", MaxConcurrent: 1, RoleModels: RoleModels{Sidecar: c.sidecarModel}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := preview.AssessResumeResultCollection(t.Context(), &ResumeSource{BranchID: "main", Session: &SessionData{Tasks: tasks}}, id)
	if err != nil || a.Disposition != RecoveryBlocked {
		t.Fatalf("preview=%+v err=%v", a, err)
	}
	if calls.Load() != 1 {
		t.Fatal("preview repeated decision inference")
	}
	c = reopenRecoveryCoordinator(t, c)
	reassessed, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil || reassessed.Disposition != RecoveryRepairClaims || reassessed.CheckpointSHA256 != a.CheckpointSHA256 {
		t.Fatalf("preview lost verified collection evidence: %+v err=%v", reassessed, err)
	}
}
