package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
)

func diagnosticTestFixture(t *testing.T) (beliefFixture, DiagnosticFixture, *DiagnosticJournal) {
	t.Helper()
	f := newBeliefFixture(t)
	second := f.model.Experiments[0]
	second.ID, second.CorrelationGroup = "second", "second_group"
	f.model.Experiments = append(f.model.Experiments, second)
	digest, err := belief.ModelDigest(f.model)
	if err != nil {
		t.Fatal(err)
	}
	f.epoch = belief.Epoch{ID: "episode-1", ModelDigest: digest, Scope: f.scope, SubjectRevision: strings.Repeat("a", 64), EnvironmentRevision: strings.Repeat("b", 64)}
	fixture := DiagnosticFixture{SchemaVersion: 1, Version: DiagnosticFixtureVersion, Model: f.model, Epoch: f.epoch,
		Revisions: belief.DiagnosticRevisions{Subject: f.epoch.SubjectRevision, Source: strings.Repeat("c", 64), Environment: f.epoch.EnvironmentRevision},
		Policy: belief.DiagnosticPolicy{SchemaVersion: 1, Version: "1", Algorithm: belief.DiagnosticGiniAlgorithm, Threshold: 900_000_000,
			Limits:     belief.DiagnosticLimits{MaxRounds: 2, MaxAttempts: 2, MaxCostUnits: 2, ActionTimeoutSeconds: 5, EpisodeTimeoutSeconds: 20, MaxEvents: 32, MaxObservationBytes: 16384, MaxFenceRetries: 3},
			Candidates: []belief.DiagnosticCandidate{{ID: "a", ExperimentID: "comparison", CostUnits: 1}, {ID: "b", ExperimentID: "second", CostUnits: 1}}},
		Measurements: []DiagnosticFixtureMeasurement{{ExperimentID: "comparison", OutcomeID: "pass", Digest: strings.Repeat("d", 64)}, {ExperimentID: "second", OutcomeID: "pass", Digest: strings.Repeat("e", 64)}}}
	j, err := NewDiagnosticJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, f.scope.BranchID, f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return f, fixture, j
}

func diagnosticView(t *testing.T, j *DiagnosticJournal) DiagnosticEpisodeState {
	t.Helper()
	views, err := j.Replay(t.Context())
	if err != nil || len(views) != 1 {
		t.Fatalf("views=%+v error=%v", views, err)
	}
	return views[0]
}

func finishDiagnostic(t *testing.T, j *DiagnosticJournal) DiagnosticEpisodeState {
	t.Helper()
	for range 10 {
		view := diagnosticView(t, j)
		if view.Status == "stopped" {
			return view
		}
		if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("fixture did not stop")
	return DiagnosticEpisodeState{}
}

func TestDiagnosticJournalEveryOfflineCommitBoundaryReopens(t *testing.T) {
	for prefix := range 6 {
		t.Run(string(rune('0'+prefix)), func(t *testing.T) {
			f, fixture, j := diagnosticTestFixture(t)
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			for range prefix {
				view := diagnosticView(t, j)
				if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
					t.Fatal(err)
				}
			}
			before := diagnosticView(t, j)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := OpenEventStore(f.workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			j, err = NewDiagnosticJournal(eventStoreJournal{store}, f.artifacts, f.tree, f.scope.BranchID, f.scope.RunID, f.scope.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if after := diagnosticView(t, j); !reflect.DeepEqual(before, after) {
				t.Fatal("reopening changed durable shadow state")
			}
			final := finishDiagnostic(t, j)
			if final.EventCount != 6 || len(final.Observations) != 2 || len(final.Decisions) != 3 || final.Decisions[2].Choice.StopReason != "threshold_reached" || final.Validation != "verified" || final.State == nil {
				t.Fatalf("final=%+v", final)
			}
			events, _ := store.ReadEvents()
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			if err := j.AdvanceFixture(t.Context(), final.Epoch.ID, final.CursorID); err == nil {
				t.Fatal("terminal episode advanced")
			}
			if !reflect.DeepEqual(final, diagnosticView(t, j)) {
				t.Fatal("replay changed terminal state")
			}
			after, _ := store.ReadEvents()
			if !reflect.DeepEqual(events, after) {
				t.Fatal("idempotent opening or replay wrote events")
			}
			v1, err := ProjectBeliefRun(t.Context(), after, f.artifacts, f.scope.RunID)
			if err != nil || len(v1.Models) != 0 || len(v1.Epochs) != 0 || len(v1.Suggestions) != 0 {
				t.Fatal("offline episodes altered v1 belief authority")
			}
		})
	}
}

func TestDiagnosticMissingAndCorruptFixturePreservesHistory(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		f, fixture, j := diagnosticTestFixture(t)
		if err := j.OpenFixture(t.Context(), fixture); err != nil {
			t.Fatal(err)
		}
		final := finishDiagnostic(t, j)
		path := filepath.Join(f.workspace, "logs", "artifacts", "data", final.FixtureRef.ID)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if corrupt {
			err = os.WriteFile(path, []byte(`{"forged":true}`), 0o600)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			t.Fatal(err)
		}
		view := diagnosticView(t, j)
		if view.State != nil || view.Validation != "unavailable" || view.Unavailable == "" || !reflect.DeepEqual(final.Decisions, view.Decisions) || !reflect.DeepEqual(final.Observations, view.Observations) || view.FixtureRef != final.FixtureRef {
			t.Fatalf("missing input fabricated or lost history: %+v", view)
		}
		if err := j.AdvanceFixture(t.Context(), fixture.Epoch.ID, view.CursorID); err == nil {
			t.Fatal("missing fixture was used")
		}
		if err := j.OpenFixture(t.Context(), fixture); err == nil {
			t.Fatal("opening silently repaired missing input")
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(final, diagnosticView(t, j)) {
			t.Fatal("restored immutable input changed history")
		}
	}
}

func TestDiagnosticReplayRejectsForgedDecisionsObservationsAndScope(t *testing.T) {
	f, fixture, j := diagnosticTestFixture(t)
	if err := j.OpenFixture(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	finishDiagnostic(t, j)
	events, _ := f.store.ReadEvents()
	di := slices.IndexFunc(events, func(e RunEvent) bool { return e.Type == EventDiagnosticEpisodeShadowed })
	oi := slices.IndexFunc(events, func(e RunEvent) bool { return e.Type == EventDiagnosticEpisodeObserved })
	for _, change := range []func([]RunEvent){
		func(es []RunEvent) { es[di].BranchID = "sibling" },
		func(es []RunEvent) { es[oi].TaskID = "child-task" },
		func(es []RunEvent) { es[oi].Attempt++ },
		func(es []RunEvent) { es[oi].Actor = "worker" },
		func(es []RunEvent) {
			var d DiagnosticShadowDecision
			_ = json.Unmarshal(es[di].Payload, &d)
			d.Choice.CandidateID = "b"
			d.ID = ""
			d.ID = beliefIdentity("hufu-diagnostic-shadow-decision-v1", d)
			es[di].Payload, _ = json.Marshal(d)
		},
		func(es []RunEvent) {
			var o diagnosticEpisodeObserved
			_ = json.Unmarshal(es[oi].Payload, &o)
			o.Observation.OutcomeID = "fail"
			es[oi].Payload, _ = json.Marshal(o)
		},
		func(es []RunEvent) {
			es[di].Payload = append(es[di].Payload[:len(es[di].Payload):len(es[di].Payload)], 'x')
		},
	} {
		copyEvents := slices.Clone(events)
		change(copyEvents)
		if _, err := ProjectDiagnosticEpisodes(t.Context(), copyEvents, f.artifacts, f.scope.RunID); err == nil {
			t.Fatal("forged event was accepted")
		}
	}
	duplicate := append(slices.Clone(events[:oi+1]), events[oi])
	if _, err := ProjectDiagnosticEpisodes(t.Context(), duplicate, f.artifacts, f.scope.RunID); err == nil {
		t.Fatal("duplicate observation advanced the episode")
	}
}

func TestDiagnosticForkHistoryIsReadOnlyAndNewEpisodesAreIsolated(t *testing.T) {
	f, fixture, j := diagnosticTestFixture(t)
	if err := j.OpenFixture(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	view := diagnosticView(t, j)
	if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
		t.Fatal(err)
	}
	view = diagnosticView(t, j)
	f.tree.Branches["child"] = &SessionBranch{ID: "child", Name: "child", ParentID: "main", ForkEventID: view.CursorID}
	f.tree.Branches["early"] = &SessionBranch{ID: "early", Name: "early", ParentID: "main", ForkEventID: f.source.ID}
	child, err := NewDiagnosticJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "child", f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view, diagnosticView(t, child)) {
		t.Fatal("fork lost inherited prefix")
	}
	if err := child.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err == nil {
		t.Fatal("fork advanced inherited episode")
	}
	finishDiagnostic(t, j)
	if !reflect.DeepEqual(view, diagnosticView(t, child)) {
		t.Fatal("fork admitted later parent events")
	}
	fixture.Epoch.Scope.BranchID, fixture.Epoch.ID = "child", "child-episode"
	if err := child.OpenFixture(t.Context(), fixture); err != nil {
		t.Fatal(err)
	}
	children, err := child.Replay(t.Context())
	if err != nil || len(children) != 2 {
		t.Fatalf("child=%+v error=%v", children, err)
	}
	if parent, err := j.Replay(t.Context()); err != nil || len(parent) != 1 {
		t.Fatal("child episode entered parent")
	}
	early, err := NewDiagnosticJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "early", f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if views, err := early.Replay(t.Context()); err != nil || len(views) != 0 {
		t.Fatal("early fork saw later opening")
	}
}

type diagnosticUncertainJournal struct {
	ConditionalEventJournal
	failType       string
	failBeforeType string
	race           bool
}

func (j *diagnosticUncertainJournal) AppendAtHead(ctx context.Context, e RunEvent, head string) (RunEvent, error) {
	if e.Type == j.failBeforeType {
		j.failBeforeType = ""
		return RunEvent{}, errors.New("injected failure before commit")
	}
	if j.race {
		j.race = false
		if _, err := j.ConditionalEventJournal.Append(ctx, RunEvent{Type: "unrelated", Actor: "runtime", BranchID: "other", RunID: "another-run"}); err != nil {
			return RunEvent{}, err
		}
	}
	durable, err := j.ConditionalEventJournal.AppendAtHead(ctx, e, head)
	if err == nil && e.Type == j.failType {
		j.failType = ""
		return RunEvent{}, errors.New("injected lost acknowledgment")
	}
	return durable, err
}

func TestDiagnosticFailedCommitDoesNotAdvanceAndOrphanArtifactCanBeReused(t *testing.T) {
	for _, kind := range []string{EventDiagnosticEpisodeOpened, EventDiagnosticEpisodeShadowed, EventDiagnosticEpisodeObserved} {
		f, fixture, j := diagnosticTestFixture(t)
		adapter := &diagnosticUncertainJournal{ConditionalEventJournal: eventStoreJournal{f.store}, failBeforeType: kind}
		j.scope.journal = adapter
		if kind == EventDiagnosticEpisodeOpened {
			if err := j.OpenFixture(t.Context(), fixture); err == nil {
				t.Fatal("failure was not injected")
			}
			if views, err := j.Replay(t.Context()); err != nil || len(views) != 0 {
				t.Fatal("orphan artifact became episode authority")
			}
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			if kind == EventDiagnosticEpisodeObserved {
				view := diagnosticView(t, j)
				if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
					t.Fatal(err)
				}
			}
			view := diagnosticView(t, j)
			if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err == nil {
				t.Fatal("failure was not injected")
			}
			if !reflect.DeepEqual(view, diagnosticView(t, j)) {
				t.Fatal("failed commit advanced projection")
			}
			if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
				t.Fatal(err)
			}
		}
		finishDiagnostic(t, j)
	}
}

func TestDiagnosticOfflineLimitsStopWithoutExecutionReservations(t *testing.T) {
	for _, limit := range []string{"attempt", "cost", "bytes", "events"} {
		t.Run(limit, func(t *testing.T) {
			_, fixture, j := diagnosticTestFixture(t)
			expected, count := "fixture_budget_exhausted", 1
			switch limit {
			case "attempt":
				fixture.Policy.Limits.MaxAttempts = 1
			case "cost":
				fixture.Policy.Limits.MaxCostUnits = 1
			case "bytes":
				fixture.Policy.Limits.MaxObservationBytes, expected, count = 1, "fixture_observation_limit", 0
			case "events":
				fixture.Policy.Threshold, fixture.Policy.Limits.MaxEvents = belief.Scale, 8
				fixture.Policy.Limits.MaxRounds, fixture.Policy.Limits.MaxAttempts, fixture.Policy.Limits.MaxCostUnits = 4, 4, 4
				for _, id := range []string{"third", "fourth"} {
					experiment := fixture.Model.Experiments[0]
					experiment.ID, experiment.CorrelationGroup = id, id
					fixture.Model.Experiments = append(fixture.Model.Experiments, experiment)
					fixture.Policy.Candidates = append(fixture.Policy.Candidates, belief.DiagnosticCandidate{ID: id, ExperimentID: id, CostUnits: 1})
					fixture.Measurements = append(fixture.Measurements, DiagnosticFixtureMeasurement{ExperimentID: id, OutcomeID: "pass", Digest: beliefIdentity("test-measurement", id)})
				}
				fixture.Epoch.ModelDigest, _ = belief.ModelDigest(fixture.Model)
				expected, count = "event_limit", 3
			}
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			view := finishDiagnostic(t, j)
			if len(view.Observations) != count || view.Decisions[len(view.Decisions)-1].Choice.StopReason != expected || view.EventCount > fixture.Policy.Limits.MaxEvents {
				t.Fatalf("limits=%+v", view)
			}
		})
	}
}

func TestDiagnosticUnknownCommitAndGlobalHeadFence(t *testing.T) {
	for _, kind := range []string{EventDiagnosticEpisodeOpened, EventDiagnosticEpisodeShadowed, EventDiagnosticEpisodeObserved} {
		f, fixture, j := diagnosticTestFixture(t)
		adapter := &diagnosticUncertainJournal{ConditionalEventJournal: eventStoreJournal{f.store}, failType: kind}
		j.scope.journal = adapter
		if kind == EventDiagnosticEpisodeOpened {
			if err := j.OpenFixture(t.Context(), fixture); err == nil {
				t.Fatal("lost acknowledgment not injected")
			}
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := j.OpenFixture(t.Context(), fixture); err != nil {
				t.Fatal(err)
			}
			if kind == EventDiagnosticEpisodeObserved {
				view := diagnosticView(t, j)
				if err := j.AdvanceFixture(t.Context(), view.Epoch.ID, view.CursorID); err != nil {
					t.Fatal(err)
				}
			}
			before := diagnosticView(t, j)
			if err := j.AdvanceFixture(t.Context(), before.Epoch.ID, before.CursorID); err == nil {
				t.Fatal("lost acknowledgment not injected")
			}
			durable := diagnosticView(t, j)
			if durable.EventCount != before.EventCount+1 {
				t.Fatal("lost acknowledgment lost durable append")
			}
			if err := j.AdvanceFixture(t.Context(), before.Epoch.ID, before.CursorID); err == nil {
				t.Fatal("stale caller advanced again")
			}
			if !reflect.DeepEqual(durable, diagnosticView(t, j)) {
				t.Fatal("stale retry changed history")
			}
		}
		before := diagnosticView(t, j)
		adapter.race = true
		if err := j.AdvanceFixture(t.Context(), before.Epoch.ID, before.CursorID); !errors.Is(err, ErrEventHeadChanged) {
			t.Fatalf("CAS race=%v", err)
		}
		if !reflect.DeepEqual(before, diagnosticView(t, j)) {
			t.Fatal("failed CAS wrote diagnostic event")
		}
		finishDiagnostic(t, j)
	}
}

func TestDiagnosticFixtureRejectsInvalidScopeAndMeasurements(t *testing.T) {
	for _, mutate := range []func(*DiagnosticFixture){
		func(f *DiagnosticFixture) { f.Model.Synthetic = false },
		func(f *DiagnosticFixture) { f.Revisions.Source = "unknown" },
		func(f *DiagnosticFixture) { f.Revisions.Subject = strings.Repeat("f", 64) },
		func(f *DiagnosticFixture) { f.Epoch.Scope.BranchID = "other" },
		func(f *DiagnosticFixture) { f.Measurements[0].OutcomeID = "timeout" },
		func(f *DiagnosticFixture) { f.Measurements[1] = f.Measurements[0] },
		func(f *DiagnosticFixture) { f.Measurements[1].Digest = f.Measurements[0].Digest },
		func(f *DiagnosticFixture) { f.Policy.Candidates[0].ExperimentID = "unknown" },
	} {
		f, fixture, j := diagnosticTestFixture(t)
		mutate(&fixture)
		if err := j.OpenFixture(t.Context(), fixture); err == nil {
			t.Fatal("invalid fixture opened")
		}
		events, _ := f.store.ReadEvents()
		if len(events) != 1 {
			t.Fatal("invalid fixture changed journal")
		}
	}
}
