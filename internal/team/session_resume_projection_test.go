package team

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func TestResumePreflightChecksActiveCheckpointProjection(t *testing.T) {
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
