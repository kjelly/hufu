package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
)

// Build real hash-chained events and immutable model/policy artifacts without
// including repeated writer-side validation in the reader benchmark setup.
func beliefHistory(t testing.TB, count int) (beliefFixture, []RunEvent) {
	t.Helper()
	f := newBeliefPolicyFixture(t)
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var binding beliefEvent
	for _, event := range events {
		if event.Type == EventBeliefModelBound {
			if err := json.Unmarshal(event.Payload, &binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendEvent := func(kind, taskID string, payload any) RunEvent {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		actor := beliefActor
		if kind == "task_completed" {
			actor = "worker"
		}
		event, err := f.store.AppendPersisted(RunEvent{Type: kind, Actor: actor, RunID: f.scope.RunID, SessionID: f.scope.SessionID, BranchID: f.scope.BranchID, TaskID: taskID, Attempt: 1, Payload: data})
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	for i := range count {
		scope := f.scope
		scope.TaskID = fmt.Sprintf("history-task-%d", i)
		receipt := ExecutionReceipt{RunID: scope.RunID, TaskID: scope.TaskID, Attempt: 1, OccurrenceAttempt: 1, ModelExecutionID: fmt.Sprintf("history-execution-%d", i), RunInputSnapshotHash: "synthetic-revision", VerifyResult: &VerificationResult{Fingerprint: "synthetic-verification", Command: "never-execute"}}
		source := appendEvent("task_completed", scope.TaskID, map[string]any{"id": scope.TaskID, "status": TaskDone, "execution_receipt": receipt})
		epoch, err := BeliefVerificationEpoch(f.model, scope, source, "")
		if err != nil {
			t.Fatal(err)
		}
		appendEvent(EventBeliefEpochOpened, scope.TaskID, beliefEvent{SchemaVersion: 1, Epoch: &epoch})
		observation, ref, err := beliefVerificationObservation(epoch, source, f.model, "comparison", "")
		if err != nil {
			t.Fatal(err)
		}
		cursor := appendEvent(EventBeliefObservationAdmitted, scope.TaskID, beliefEvent{SchemaVersion: 1, EpochID: epoch.ID, Observation: &observation, Source: &ref})
		state, err := belief.Project(f.model, epoch, []belief.Observation{observation})
		if err != nil {
			t.Fatal(err)
		}
		input := beliefPolicyContext{epoch: BeliefEpochState{Epoch: epoch, State: &state, Observations: []belief.Observation{observation}}, binding: binding, cursor: cursor, actor: source.Actor}
		record, err := buildBeliefSuggestion(t.Context(), input, f.artifacts, 1)
		if err != nil {
			t.Fatal(err)
		}
		appendEvent(EventBeliefPolicySuggested, scope.TaskID, record)
		appendEvent(EventBeliefEpochClosed, scope.TaskID, beliefEvent{SchemaVersion: 1, EpochID: epoch.ID, TriggerID: source.ID, Reason: "scope_closed"})
	}
	if err := f.store.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	events, err = f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return f, events
}

type countingBeliefArtifacts struct {
	ArtifactStore
	resolves int
}

func (s *countingBeliefArtifacts) Resolve(ctx context.Context, ref ArtifactRef) (ArtifactRef, error) {
	s.resolves++
	return s.ArtifactStore.Resolve(ctx, ref)
}

func TestBeliefHistoryReplayWorkGrowsLinearly(t *testing.T) {
	for _, count := range []int{8, 32, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, events := beliefHistory(t, count)
			artifacts := &countingBeliefArtifacts{ArtifactStore: f.artifacts}
			views, err := ProjectBeliefSuggestions(t.Context(), events, artifacts, f.scope.RunID)
			if err != nil || len(views) != count {
				t.Fatalf("views=%d error=%v", len(views), err)
			}
			// One model binding and one policy read per recommendation. The old
			// prefix replay additionally re-read the model for every suggestion.
			if artifacts.resolves != count+1 {
				t.Fatalf("artifact reads=%d want=%d", artifacts.resolves, count+1)
			}
			for _, view := range views {
				if view.Validation != "verified" || view.Record.SelectedCandidateID != "check-b" {
					t.Fatalf("history was not validated: %+v", view)
				}
			}
		})
	}
}

func TestBeliefStreamingHistoryPreservesEarlierSnapshots(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	first, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	initial := f.state(t)
	if err := f.journal.Retract(t.Context(), f.epoch.ID, initial.Observations[0].ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	second, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.CloseEpoch(t.Context(), f.epoch.ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	events, err := f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	views, err := ProjectBeliefSuggestions(t.Context(), events, f.artifacts, f.scope.RunID)
	if err != nil || len(views) != 2 {
		t.Fatalf("views=%+v error=%v", views, err)
	}
	if !reflect.DeepEqual(views[0].Record, first) || !reflect.DeepEqual(views[1].Record, second) || first.StateDigest == second.StateDigest {
		t.Fatal("later retraction replaced a historical snapshot")
	}
	for i, event := range events {
		if event.Type != EventBeliefPolicySuggested {
			continue
		}
		var record BeliefPolicySuggestion
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			t.Fatal(err)
		}
		// Bind each stored generation independently to its own historical
		// prefix, including the initial actor and admission/retraction cursor.
		input, err := policyContext(t.Context(), events[:i], f.artifacts, f.scope.RunID, f.epoch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if input.cursor.ID != record.SourceCursorID || input.actor != record.SourceActor || beliefPolicySnapshotDigest(input.epoch, input.actor) != record.SnapshotDigest {
			t.Fatal("streamed history differs from its prefix")
		}
	}
	r, err := reduceBeliefRun(t.Context(), events, f.artifacts, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	one := r.epochs[f.epoch.ID].snapshot()
	two := r.epochs[f.epoch.ID].snapshot()
	if !reflect.DeepEqual(one, two) || len(two.Retracted) != 1 {
		t.Fatal("snapshot duplicated retained sources or tombstones")
	}
}

func BenchmarkBeliefHistoryReplay(b *testing.B) {
	for _, count := range []int{16, 64, 256} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			f, events := beliefHistory(b, count)
			b.ReportAllocs()
			for b.Loop() {
				views, err := ProjectBeliefSuggestions(b.Context(), events, f.artifacts, f.scope.RunID)
				if err != nil || len(views) != count {
					b.Fatalf("views=%d error=%v", len(views), err)
				}
			}
		})
	}
}

func TestBeliefSettledAdmissionRequiresLocalClosure(t *testing.T) {
	f := newBeliefFixture(t)
	f.bind(t)
	f.admit(t)
	events, err := f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	key := f.state(t).Sources[0].ReceiptKey
	if settledBeliefAdmissions(events, "main")[key] {
		t.Fatal("partial admission suppressed close repair")
	}
	if !settledBeliefAdmissions(events, "child")[key] {
		t.Fatal("inherited admission lost ancestor isolation")
	}
	if err := admitBeliefSource(t.Context(), f.journal, f.model, "comparison", f.source, settledBeliefAdmissions(events, "main")); err != nil {
		t.Fatal(err)
	}
	events, err = f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	settled := settledBeliefAdmissions(events, "main")
	if !settled[key] {
		t.Fatal("closed local receipt was not settled")
	}
	// This journal rejects any I/O: a settled receipt must skip writer-side
	// replay, not merely rely on append idempotency after expensive reads.
	writer := &BeliefJournal{branchID: f.scope.BranchID}
	if err := admitBeliefSource(t.Context(), writer, f.model, "comparison", f.source, settled); err != nil {
		t.Fatal(err)
	}
}

func TestBeliefSourcesHaveDeterministicReceiptTies(t *testing.T) {
	f := newBeliefFixture(t)
	experiment := f.model.Experiments[0]
	experiment.ID = "second"
	f.model.Experiments = append(f.model.Experiments, experiment)
	var err error
	f.epoch, err = BeliefVerificationEpoch(f.model, f.scope, f.source, "")
	if err != nil {
		t.Fatal(err)
	}
	f.bind(t)
	f.admit(t)
	alias, err := f.store.AppendPersisted(RunEvent{Type: f.source.Type, TaskID: f.source.TaskID, Actor: f.source.Actor, Payload: f.source.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.AdmitVerification(t.Context(), f.epoch.ID, alias.ID, "second", ""); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.Sources) != 2 || state.State.Status != belief.DependenceUnresolved {
		t.Fatalf("state=%+v", state)
	}
	if state.Sources[0].ReceiptKey != state.Sources[1].ReceiptKey || state.Sources[0].EventID >= state.Sources[1].EventID {
		t.Fatal("same-receipt sources lack deterministic event ordering")
	}
	for range 20 {
		if got := f.state(t); !reflect.DeepEqual(got, state) {
			t.Fatal("map iteration changed source projection")
		}
	}
}
