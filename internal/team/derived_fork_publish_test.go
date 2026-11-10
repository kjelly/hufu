package team

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDerivedForkPublicationCommitsSnapshotAndIntent(t *testing.T) {
	f, ops := derivedForkPreparationFixture(t)
	f.tree.Branches["main"].State.TaskPlan = []*TodoItem{{ID: "historical-task", Agent: "worker", Desc: "parent-only instructions"}}
	f.tree.Branches["main"].State.ActiveModel = "parent-model"
	if err := SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	previousHook := workspaceVersionStageHook
	t.Cleanup(func() { workspaceVersionStageHook = previousHook })
	workspaceVersionStageHook = func(stage string) error {
		if stage == "fork:child_saved" {
			child := f.tree.GetBranch("investigation")
			if child == nil || len(child.State.TaskPlan) != 0 || child.State.ActiveModel != "" {
				t.Error("unpublished derived child copied parent execution state")
			}
		}
		return nil
	}
	before := mustReadEvents(t, f.events)
	result, err := ops.ForkNewTask(t.Context(), "investigation", "main")
	if err != nil {
		t.Fatal(err)
	}
	events := mustReadEvents(t, f.events)
	if len(events) != len(before)+2 || !reflect.DeepEqual(before, events[:len(before)]) {
		t.Fatal("fork rewrote its source or did not publish exactly two native events")
	}
	commit, created := events[len(before)], events[len(before)+1]
	if commit.Type != string(EventWorkspaceSnapshotCommitted) || created.Type != string(EventDerivedForkCreated) ||
		created.PreviousID != commit.ID || created.RunID != "" || result.Snapshot.CommitEventID != commit.ID {
		t.Fatal("publication split the commit or invented an execution run")
	}
	payload, err := decodeDerivedForkCreated(created)
	if err != nil || payload.SnapshotID != string(result.Snapshot.ID) || result.Branch.Derivation == nil || payload.Intent != *result.Branch.Derivation {
		t.Fatalf("publication lost its original branch intent: %v", err)
	}
	again, err := f.events.AppendPersistedContext(t.Context(), created)
	if err != nil || !reflect.DeepEqual(created, again) || len(mustReadEvents(t, f.events)) != len(events) {
		t.Fatalf("exact creation replay changed durable identity: %v", err)
	}
	payload.Intent.TaskIDFloor++
	forged := created
	forged.Payload, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.events.AppendPersistedContext(t.Context(), forged); !errors.Is(err, ErrDerivedForkIntent) {
		t.Fatalf("creation replay replaced frozen metadata: %v", err)
	}
}

func TestDerivedForkPublicationFailureRecoversAsOneOperation(t *testing.T) {
	for _, failure := range []string{"second_event", "uncertain_sync", "after_commit"} {
		t.Run(failure, func(t *testing.T) {
			f, ops := derivedForkPreparationFixture(t)
			before := mustReadEvents(t, f.events)
			previousHook := workspaceVersionStageHook
			t.Cleanup(func() { workspaceVersionStageHook = previousHook })
			if failure == "second_event" {
				if _, err := f.events.store.DB().ExecContext(t.Context(), `CREATE TRIGGER refuse_derived_publication BEFORE INSERT ON events WHEN NEW.type='derived_fork_created' BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`); err != nil {
					t.Fatal(err)
				}
			}
			workspaceVersionStageHook = func(stage string) error {
				if stage == "fork:before_events" && failure == "uncertain_sync" {
					f.events.commitHook = func() error { return errSimulatedCrash }
				}
				if stage == "fork:event_committed" && failure == "after_commit" {
					return errSimulatedCrash
				}
				return nil
			}
			_, forkErr := ops.ForkNewTask(t.Context(), "investigation", "main")
			if forkErr == nil || (failure == "second_event" && !strings.Contains(forkErr.Error(), "fixture rejection")) ||
				(failure != "second_event" && !errors.Is(forkErr, errSimulatedCrash)) {
				t.Fatalf("publication failed at the wrong boundary: %v", forkErr)
			}
			workspaceVersionStageHook = previousHook
			f.reopen()
			after := mustReadEvents(t, f.events)
			wantEvents := len(before) + 2
			if failure == "second_event" {
				wantEvents = len(before)
			}
			if len(after) != wantEvents || !reflect.DeepEqual(before, after[:len(before)]) {
				t.Fatal("publication retained half its native events or rewrote the parent")
			}
			report, err := RecoverWorkspaceOperations(t.Context(), f.ops())
			if err != nil {
				t.Fatal(err)
			}
			if failure == "second_event" {
				if len(report.Removed) != 1 || f.tree.GetBranch("investigation") != nil || f.tree.ActiveBranch != "main" {
					t.Fatal("eventless incomplete child was not removed")
				}
			} else if len(report.Completed) != 1 || f.tree.ActiveBranch != "investigation" || f.tree.GetBranch("investigation").Derivation == nil {
				t.Fatal("durable derivation was not completed forward")
			}
			repeated, err := RecoverWorkspaceOperations(t.Context(), f.ops())
			if err != nil || !repeated.Empty() || !reflect.DeepEqual(after, mustReadEvents(t, f.events)) {
				t.Fatalf("recovery repeated publication or appended recovery evidence: %v", err)
			}
		})
	}
}
