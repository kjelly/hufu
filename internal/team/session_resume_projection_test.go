package team

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

func TestResumePreflightChecksActiveCheckpointProjection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, name := range []string{"matched", "empty", "prefix", "progress_mismatch", "result_mismatch", "recorded_resolved", "unrecorded", "wrong_reason", "unrelated_barrier", "pending_terminal", "inactive_branch"} {
		t.Run(name, func(t *testing.T) {
			c, f, source := metadataContractRecoveryFixture(t)
			ctx := WithMetadataOnlyContractRecovery(t.Context())
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "checked-projection", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(f.control, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			if name != "inactive_branch" {
				if err := activateMetadataResumeSource(t.Context(), f.control, child); err != nil {
					t.Fatal(err)
				}
			} else {
				parent, err := ReadResumeSource(f.control, "main", "")
				if err != nil {
					t.Fatal(err)
				}
				if err := activateMetadataResumeSource(t.Context(), f.control, parent); err != nil {
					t.Fatal(err)
				}
			}
			checkpoint := LoadSession(f.control)
			const reason = "event-store projection mismatch: old progress reducer"
			wantError := false
			switch name {
			case "empty":
				checkpoint = NewSession()
			case "prefix":
				checkpoint.Tasks = checkpoint.Tasks[:1]
			case "progress_mismatch", "inactive_branch":
				checkpoint.Tasks[0].ActionProgress = &ActionProgressObservation{ActionID: "different-action", Attempt: 2}
				wantError = name == "progress_mismatch"
			case "result_mismatch":
				checkpoint.Tasks[0].TypedResult.Summary = "different accepted work"
				wantError = true
			case "recorded_resolved", "unrecorded", "wrong_reason", "unrelated_barrier", "pending_terminal":
				checkpoint.RecoveryRequired, checkpoint.RecoveryReason = true, reason
				if name != "unrecorded" {
					f.events.SetBranchID(branch)
					body, err := json.Marshal(map[string]string{"reason": reason})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.events.AppendPersisted(RunEvent{Type: "projection_mismatch", Actor: "coordinator", Payload: body}); err != nil {
						t.Fatal(err)
					}
				}
				if name == "wrong_reason" {
					checkpoint.RecoveryReason += " changed"
				}
				if name == "unrelated_barrier" {
					checkpoint.RecoveryReason = "cost ledger replay failed"
				}
				if name == "pending_terminal" {
					checkpoint.PendingTerminalCommit = &PendingTerminalCommit{}
				}
				wantError = name != "recorded_resolved"
			}
			if err := SaveSession(f.control, checkpoint); err != nil {
				t.Fatal(err)
			}
			child, err = ReadResumeSource(f.control, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			beforeEvents := mustReadEvents(t, f.events)
			beforeCheckpoint := LoadSession(f.control)
			beforeSource, err := collectionDigest(child.Session)
			if err != nil {
				t.Fatal(err)
			}
			preview := newTestCoordinator(t)
			preview.executionRunID = ""
			preview.session, preview.providerManager, preview.maxConcurrent, preview.projectDir = c.session, c.providerManager, c.maxConcurrent, c.projectDir
			assessment, err := preview.AssessResumeSource(t.Context(), child)
			if wantError {
				if err == nil || !strings.Contains(err.Error(), "checkpoint") {
					t.Fatalf("checkpoint mismatch not diagnosed: assessment=%+v err=%v", assessment, err)
				}
			} else if err != nil || assessment.Accepted != 1 || assessment.Resumable != 2 {
				t.Fatalf("valid checkpoint refused: assessment=%+v err=%v", assessment, err)
			}
			afterSource, err := collectionDigest(child.Session)
			if err != nil || beforeSource != afterSource ||
				!reflect.DeepEqual(beforeEvents, mustReadEvents(t, f.events)) ||
				!reflect.DeepEqual(beforeCheckpoint, LoadSession(f.control)) {
				t.Fatal("preview changed checkpoint, source or events")
			}
		})
	}
}

func TestResumePreflightClearsProvenRedundantFailureOutputMismatch(t *testing.T) {
	t.Parallel()
	c := newFailureOutputCoordinator(t)
	items, err := c.CommitTaskCreation(t.Context(), []TodoSpec{{Agent: "worker", Desc: "preserve sealed work"}})
	if err != nil {
		t.Fatal(err)
	}
	output := strings.Repeat("complete sealed work\n", 500)
	if err := c.CommitTaskTransition(t.Context(), items[0].ID, TaskPending, TaskBlocked, "protocol failure", output, map[string]any{"failure_event": &FailureEventPayload{TaskID: items[0].ID, FailureClass: FailureProtocol, Summary: "protocol failure"}}); err != nil {
		t.Fatal(err)
	}
	item := c.todoItemByID(items[0].ID)
	legacy := taskTransitionPayload(item)
	delete(legacy, "output_complete")
	legacy["output"] = utils.TruncateString(output, 2000)
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.eventStore.AppendPersisted(RunEvent{Type: "task_blocked", TaskID: item.ID, Actor: "worker", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	const reason = "event-store projection mismatch: legacy failure snapshot truncated output"
	raw, err = json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.eventStore.AppendPersisted(RunEvent{Type: "projection_mismatch", Actor: "coordinator", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	checkpoint := NewSession()
	checkpoint.Tasks = []*TodoItem{item}
	checkpoint.RecoveryRequired, checkpoint.RecoveryReason = true, reason
	if err := SaveSession(c.session.Workspace, checkpoint); err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, c.eventStore)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestCoordinator(t, withCoordinatorWorkspace(c.session.Workspace))
	reopened.sessionData = LoadSession(c.session.Workspace)
	reopened.initEventStore()
	if reopened.sessionData.RecoveryRequired || reopened.todoItemByID(item.ID).Output != output {
		t.Fatal("proven canonical output was not restored on ordinary startup")
	}
	after := mustReadEvents(t, reopened.eventStore)
	if !reflect.DeepEqual(before, after[:len(before)]) || CompareCanonicalProjection(reopened.sessionData, after) != nil {
		t.Fatal("restart rewrote history or weakened canonical parity")
	}
}

func TestResultCollectionRecoveryPreviewRejectsCheckpointDrift(t *testing.T) {
	t.Parallel()
	c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), SideEffectNone)
	if source.activeBranch != source.BranchID {
		t.Fatal("fixture must select the active branch")
	}
	checkpoint := *source.Session
	checkpoint.Tasks = make([]*TodoItem, len(source.Session.Tasks))
	for i, item := range source.Session.Tasks {
		checkpoint.Tasks[i] = cloneTodoItem(item)
	}
	checkpoint.Tasks[0].Output = "unverified checkpoint replacement"
	if err := SaveSession(c.session.Workspace, &checkpoint); err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, c.eventStore)
	preview := newTestCoordinator(t)
	preview.executionRunID, preview.session = "", c.session
	if _, err := preview.AssessResumeResultCollection(t.Context(), source, id); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("result-only preview promised admission with checkpoint drift: %v", err)
	}
	if !reflect.DeepEqual(before, mustReadEvents(t, c.eventStore)) || LoadSession(c.session.Workspace).Tasks[0].Output != checkpoint.Tasks[0].Output {
		t.Fatal("rejected preview changed checkpoint or history")
	}
}
