package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
)

type beliefFixture struct {
	workspace string
	store     *EventStore
	artifacts *FileArtifactStore
	journal   *BeliefJournal
	tree      *SessionTree
	model     belief.Model
	scope     belief.Scope
	source    RunEvent
	epoch     belief.Epoch
}

func newBeliefFixture(t *testing.T) beliefFixture {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../belief/testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	journal, err := NewBeliefJournal(eventStoreJournal{store}, artifacts, tree, "main", "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	scope := belief.Scope{RunID: "run-1", BranchID: "main", SessionID: "session-1", TaskID: "task-1", Attempt: 1}
	receipt := ExecutionReceipt{RunID: scope.RunID, TaskID: scope.TaskID, Attempt: 1, OccurrenceAttempt: 1, ModelExecutionID: "execution-1", RunInputSnapshotHash: "synthetic-input-revision", VerifyResult: &VerificationResult{ExitCode: 0, Fingerprint: "vfp-synthetic", Command: "must-never-execute", WorkDir: "synthetic-environment"}}
	payload, err := json.Marshal(map[string]any{"id": scope.TaskID, "status": TaskDone, "execution_receipt": receipt})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AppendPersisted(RunEvent{Type: "task_completed", TaskID: scope.TaskID, Actor: "worker", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := BeliefVerificationEpoch(model, scope, source, "")
	if err != nil {
		t.Fatal(err)
	}
	return beliefFixture{workspace, store, artifacts, journal, tree, model, scope, source, epoch}
}
func (f beliefFixture) bind(t *testing.T) {
	t.Helper()
	if err := f.journal.BindModel(t.Context(), f.model); err != nil {
		t.Fatal(err)
	}
	if err := f.journal.OpenEpoch(t.Context(), f.epoch); err != nil {
		t.Fatal(err)
	}
}
func (f beliefFixture) admit(t *testing.T) {
	t.Helper()
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, f.source.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
}
func (f beliefFixture) state(t *testing.T) BeliefEpochState {
	t.Helper()
	states, err := f.journal.Replay(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("states=%+v", states)
	}
	return states[0]
}

func TestBeliefJournalDurableReplayAndIdempotency(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	f.admit(t)
	state := f.state(t)
	if state.Unavailable != "" || state.State == nil || (*state.State.Distribution)[0].Units != 142857143 || len(state.Sources) != 1 {
		t.Fatalf("state=%+v", state)
	}
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	before := len(events)
	for range 100 {
		f.admit(t)
	}
	after, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != before || f.state(t).State.Digest != state.State.Digest {
		t.Fatal("duplicate receipt changed belief")
	}
	// The exact immutable receipt repeated in another task event remains one observation.
	another, err := f.store.AppendPersisted(RunEvent{Type: f.source.Type, TaskID: f.source.TaskID, Actor: f.source.Actor, Payload: f.source.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, another.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.state(t).Sources) != 1 {
		t.Fatal("receipt alias became independent source")
	}
	reader, err := OpenFileArtifactStoreReadOnly(f.workspace, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectBeliefEpochs(t.Context(), after, reader, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if projected[0].State.Digest != state.State.Digest {
		t.Fatal("replay differed from live state")
	}
}
func TestBeliefRetractionTombstoneAndPrior(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	expected, _, err := beliefVerificationObservation(f.epoch, f.source, f.model, "comparison", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.Retract(t.Context(), f.epoch.ID, expected.ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	f.admit(t)
	state := f.state(t)
	if state.State.Status != belief.Prior || len(state.State.ObservationIDs) != 0 || len(state.Retracted) != 1 {
		t.Fatal("late observation revived tombstone")
	}
	prior, err := belief.Project(f.model, f.epoch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.State.Digest != prior.Digest {
		t.Fatal("withdrawal did not reconstruct prior")
	}
}
func TestBeliefMissingModelDoesNotUseCurrentSettings(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	f.admit(t)
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == EventBeliefModelBound {
			var payload beliefEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.workspace, "logs", "artifacts", "data", payload.ModelRef.ID)); err != nil {
				t.Fatal(err)
			}
		}
	}
	states, err := ProjectBeliefEpochs(t.Context(), events, f.artifacts, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if states[0].State != nil || states[0].Unavailable != "model_unavailable" {
		t.Fatalf("missing model=%+v", states)
	}
}
func TestBeliefSourceForgeryAndClosure(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, "invented-event", "comparison", ""); err == nil {
		t.Fatal("forged source admitted")
	}
	badEpoch := f.epoch
	badEpoch.ID = "wrong-scope"
	badEpoch.Scope.BranchID = "sibling"
	if err := f.journal.OpenEpoch(t.Context(), badEpoch); err == nil {
		t.Fatal("cross-branch epoch opened")
	}
	f.admit(t)
	if err := f.journal.CloseEpoch(t.Context(), f.epoch.ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if !f.state(t).Closed {
		t.Fatal("epoch not closed")
	}
	another, err := f.store.AppendPersisted(RunEvent{Type: f.source.Type, TaskID: f.source.TaskID, Actor: f.source.Actor, Payload: f.source.Payload})
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate is an idempotent lookup even after closure.
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, another.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
}
func TestBeliefReplayBranchLineageAndSourceAdmissionGap(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	// Crash before admission: reconstruct the writer, then admit from the original durable source.
	recovered, err := NewBeliefJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "main", f.scope.RunID, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.AdmitVerification(t.Context(), f.epoch.ID, f.source.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	mainState := f.state(t)
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	fork := events[len(events)-1]
	f.tree.Branches["child"] = &SessionBranch{ID: "child", Name: "child", ParentID: "main", ForkEventID: fork.ID}
	f.tree.Branches["sibling"] = &SessionBranch{ID: "sibling", Name: "sibling", ParentID: "main", ForkEventID: f.source.ID}
	// A later main retraction must not enter the child or sibling projection.
	if err := f.journal.Retract(t.Context(), f.epoch.ID, mainState.State.ObservationIDs[0], f.source.ID); err != nil {
		t.Fatal(err)
	}
	events, err = f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	childEvents, err := ProjectValidatedEventsForBranch(events, f.tree, "child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := ProjectBeliefEpochs(t.Context(), childEvents, f.artifacts, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child[0].State.Digest != mainState.State.Digest {
		t.Fatal("ancestor-after-fork event entered child")
	}
	siblingEvents, err := ProjectValidatedEventsForBranch(events, f.tree, "sibling")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := ProjectBeliefEpochs(t.Context(), siblingEvents, f.artifacts, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sibling) != 0 {
		t.Fatal("sibling saw post-fork binding")
	}
}

type uncertainBeliefJournal struct {
	EventJournal
	fail bool
}

func (j *uncertainBeliefJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	durable, err := j.EventJournal.Append(ctx, event)
	if err == nil && event.Type == EventBeliefObservationAdmitted && j.fail {
		j.fail = false
		return RunEvent{}, errors.New("durability unknown")
	}
	return durable, err
}
func TestBeliefUnknownCommitUsesSameAdmission(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	uncertain := &uncertainBeliefJournal{EventJournal: eventStoreJournal{f.store}, fail: true}
	writer, err := NewBeliefJournal(uncertain, f.artifacts, f.tree, "main", f.scope.RunID, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AdmitVerification(t.Context(), f.epoch.ID, f.source.ID, "comparison", ""); err == nil {
		t.Fatal("missing injected durability error")
	}
	if err := writer.AdmitVerification(t.Context(), f.epoch.ID, f.source.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	states, err := writer.Replay(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states[0].Sources) != 1 {
		t.Fatal("unknown commit duplicated source")
	}
	before, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Replay(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("replay appended events")
	}
}

func TestBeliefRejectsForgedOutcomeAndRevision(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	wrong := f.epoch
	wrong.ID = "wrong-revision"
	wrong.SubjectRevision = "changed"
	if err := f.journal.OpenEpoch(t.Context(), wrong); err != nil {
		t.Fatal(err)
	}
	if err := f.journal.AdmitVerification(t.Context(), wrong.ID, f.source.ID, "comparison", ""); err == nil {
		t.Fatal("accepted old receipt for changed subject")
	}
	f.admit(t)
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.Type != EventBeliefObservationAdmitted {
			continue
		}
		var payload beliefEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payload.Observation.OutcomeID = "fail"
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		events[i].Payload = data
	}
	if _, err := ProjectBeliefEpochs(t.Context(), events, f.artifacts, f.scope.RunID); err == nil {
		t.Fatal("accepted claimed outcome inconsistent with receipt")
	}
}
func TestBeliefDistinctInvocationsDoNotCreateIndependence(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	f.admit(t)
	var source struct {
		Receipt ExecutionReceipt `json:"execution_receipt"`
	}
	if err := json.Unmarshal(f.source.Payload, &source); err != nil {
		t.Fatal(err)
	}
	source.Receipt.ModelExecutionID = "execution-2"
	data, err := json.Marshal(map[string]any{"id": f.scope.TaskID, "status": TaskDone, "execution_receipt": source.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	event, err := f.store.AppendPersisted(RunEvent{Type: "task_completed", TaskID: f.scope.TaskID, Actor: "worker", Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, event.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if state.State.Status != belief.DependenceUnresolved || state.State.Distribution != nil || len(state.Sources) != 2 {
		t.Fatal("different invocation was assumed independent")
	}
}

func TestBeliefEventScopeCannotBeRebound(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	f.admit(t)
	original, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session", "attempt", "task", "branch"} {
		t.Run(field, func(t *testing.T) {
			events := append([]RunEvent(nil), original...)
			for i := range events {
				if events[i].Type != EventBeliefObservationAdmitted {
					continue
				}
				switch field {
				case "session":
					events[i].SessionID = "other"
				case "attempt":
					events[i].Attempt++
				case "task":
					events[i].TaskID = "other"
				case "branch":
					events[i].BranchID = "other"
				}
			}
			if _, err := ProjectBeliefEpochs(t.Context(), events, f.artifacts, f.scope.RunID); err == nil {
				t.Fatal("accepted rebound belief event")
			}
		})
	}
}
