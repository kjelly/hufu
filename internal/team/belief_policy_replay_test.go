package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
)

func beliefBoundArtifactPath(t *testing.T, f beliefFixture, policy bool) string {
	t.Helper()
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventBeliefModelBound {
			continue
		}
		var payload beliefEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		ref := payload.ModelRef
		if policy {
			ref = payload.PolicyRef
		}
		return filepath.Join(f.workspace, "logs", "artifacts", "data", ref.ID)
	}
	t.Fatal("missing bound artifact")
	return ""
}

func TestBeliefPolicyMissingArtifactsPreserveHistoricalDecision(t *testing.T) {
	for _, policyMissing := range []bool{false, true} {
		t.Run(map[bool]string{false: "model", true: "policy"}[policyMissing], func(t *testing.T) {
			f := newBeliefPolicyFixture(t)
			first, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 0)
			if err != nil {
				t.Fatal(err)
			}
			path := beliefBoundArtifactPath(t, f, policyMissing)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			views := f.policyViews(t)
			if len(views) != 1 || views[0].Validation != "unavailable" || !reflect.DeepEqual(views[0].Record, first) {
				t.Fatalf("missing artifact replaced history: %+v", views)
			}
			// Available policy grants are still checked when only math is missing.
			if !policyMissing {
				events, err := f.store.ReadEvents()
				if err != nil {
					t.Fatal(err)
				}
				i := slices.IndexFunc(events, func(e RunEvent) bool { return e.Type == EventBeliefPolicySuggested })
				forged := first
				forged.SelectedCandidateID = "invented-action"
				events[i].Payload, err = json.Marshal(forged)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ProjectBeliefSuggestions(t.Context(), events, f.artifacts, f.scope.RunID); err == nil {
					t.Fatal("missing model excused an unauthorized candidate")
				}
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			views = f.policyViews(t)
			if views[0].Validation != "verified" || !reflect.DeepEqual(views[0].Record, first) {
				t.Fatal("restoring the locked artifact rewrote history")
			}
		})
	}
}

func TestBeliefPolicyRecordedUnavailabilitySurvivesArtifactRestore(t *testing.T) {
	for _, policyMissing := range []bool{false, true} {
		f := newBeliefPolicyFixture(t)
		path := beliefBoundArtifactPath(t, f, policyMissing)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		recorded, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 0)
		if err != nil || recorded.SelectedCandidateID != "" || recorded.Unavailable == "" {
			t.Fatalf("missing artifact suggested an action: %+v %v", recorded, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		views := f.policyViews(t)
		if len(views) != 1 || !reflect.DeepEqual(views[0].Record, recorded) || views[0].Validation != "verified" {
			t.Fatalf("restoration changed recorded availability fact: %+v", views)
		}
	}
}

func TestBeliefPolicyUnresolvedAndUnauthorizedNeverSelect(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	policy, err := CompileBeliefPolicy(f.model, beliefTestPolicy(), beliefTestCatalog(t, "worker"))
	if err != nil {
		t.Fatal(err)
	}
	state := f.state(t).State
	for _, actor := range []string{"intruder", ""} {
		advice, err := boundBeliefAdvice(&policy, state, actor, "")
		if err != nil || advice.Unavailable != "candidate_not_authorized" || advice.CandidateID != "" {
			t.Fatalf("unauthorized advice=%+v error=%v", advice, err)
		}
	}
	unresolved := *state
	unresolved.Status = belief.DependenceUnresolved
	unresolved.Distribution = nil
	advice, err := boundBeliefAdvice(&policy, &unresolved, "worker", "")
	if err != nil || advice.Unavailable != "distribution_unavailable" || advice.CandidateID != "" {
		t.Fatal("unresolved belief produced a candidate")
	}
	if _, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), maxBeliefAdviceGenerations); err == nil {
		t.Fatal("exhausted generation budget accepted")
	}
}

func TestBeliefPolicyBranchLineageAndGlobalAppendFence(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	first, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	f.tree.Branches["child"] = &SessionBranch{ID: "child", Name: "child", ParentID: "main", ForkEventID: events[len(events)-1].ID}
	child, err := NewBeliefJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "child", f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := child.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	views, err := ProjectBeliefSuggestions(t.Context(), visible, f.artifacts, f.scope.RunID)
	if err != nil || len(views) != 1 || !reflect.DeepEqual(views[0].Record, first) {
		t.Fatal("child lost inherited advisory history")
	}
	if _, err := child.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 1); err == nil {
		t.Fatal("child wrote to an ancestor epoch")
	}
	// A sibling's newer global chain head is not part of main's visible
	// lineage, but it is the correct transactional append fence.
	if _, err := f.store.AppendPersisted(RunEvent{Type: "sibling-note", Actor: "runtime", BranchID: "child", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	parent, err := NewBeliefJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "main", f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	f.journal = parent
	if err := parent.Retract(t.Context(), f.epoch.ID, f.state(t).Observations[0].ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	// Put another sibling event after the last visible parent event.
	if _, err := f.store.AppendPersisted(RunEvent{Type: "sibling-note", Actor: "runtime", BranchID: "child", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 1); err != nil {
		t.Fatalf("invisible sibling tail broke a valid append: %v", err)
	}
	visible, err = child.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	views, err = ProjectBeliefSuggestions(t.Context(), visible, f.artifacts, f.scope.RunID)
	if err != nil || len(views) != 1 {
		t.Fatal("parent advice beyond the fork cursor entered the child")
	}
}

func TestBeliefPolicyDuplicateAdmissionKeepsOriginalActor(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	snapshot := f.policySnapshot(t)
	first, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0)
	if err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	alias, err := f.store.AppendPersisted(RunEvent{Type: f.source.Type, TaskID: f.source.TaskID, Actor: "other-worker", Payload: f.source.Payload})
	if err != nil {
		t.Fatal(err)
	}
	// Replay must tolerate a duplicate fact without replacing its first
	// provenance, even if an owner previously appended it with a different key.
	source := state.Sources[0]
	source.EventID, source.EventHash = alias.ID, alias.Hash
	if err := f.journal.append(t.Context(), EventBeliefObservationAdmitted, f.scope.TaskID, f.scope.Attempt, "duplicate-source-alias", beliefEvent{EpochID: f.epoch.ID, Observation: &state.Observations[0], Source: &source}); err != nil {
		t.Fatal(err)
	}
	if f.policySnapshot(t) != snapshot || f.state(t).Sources[0] != state.Sources[0] {
		t.Fatal("duplicate admission changed the evidence owner or policy input")
	}
	retried, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 1)
	if err != nil || !reflect.DeepEqual(first, retried) || len(f.policyViews(t)) != 1 {
		t.Fatalf("duplicate citation changed advisory authorization: %+v %v", retried, err)
	}
}
