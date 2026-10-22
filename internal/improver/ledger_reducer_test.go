package improver

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

func TestLedgerRejectsInvalidEventsAndPinsBranch(t *testing.T) {
	f := newControllerFixture(t, false)
	lease := f.bound(t, "probe_0001", true)
	events, err := f.journal.ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, err := ReplayLedger(events, f.scope, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"generation", "owner", "identity", "clock", "unknown_field", "unknown_event", "scope"} {
		t.Run(change, func(t *testing.T) {
			copyEvents := slices.Clone(events)
			last := copyEvents[len(copyEvents)-1]
			payload := stageStarted{eventMeta{1, f.scope.ExperimentID, f.clock.Now()}, lease}
			switch change {
			case "generation":
				payload.Generation++
			case "owner":
				payload.Owner = "different-owner"
			case "identity":
				last.Type = eventBound
				last.Payload, err = paired.CanonicalJSON(stageBound{payload.eventMeta, lease, paired.RuntimeIdentity{InvocationID: "different", RunID: "different", BranchID: "different"}})
			case "clock":
				payload.ObservedAt = payload.ObservedAt.Add(-time.Hour)
			case "unknown_field":
				var fields map[string]any
				if err := json.Unmarshal(last.Payload, &fields); err != nil {
					t.Fatal(err)
				}
				fields["extra"] = true
				last.Payload, err = paired.CanonicalJSON(fields)
			case "unknown_event":
				last.Type = "improvement_unsupported"
			case "scope":
				last.SessionID = "wrong-session"
			}
			if err != nil {
				t.Fatal(err)
			}
			if change != "identity" && change != "unknown_field" {
				last.Payload, err = paired.CanonicalJSON(payload)
			}
			if err != nil {
				t.Fatal(err)
			}
			copyEvents[len(copyEvents)-1] = last
			if _, err := ReplayLedger(copyEvents, f.scope, f.plan); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
	// A sibling branch's same experiment ID cannot affect this projection.
	sibling := events[len(events)-1]
	sibling.BranchID = "sibling-branch"
	sibling.ID = "sibling-head"
	sibling.Payload = []byte(`{"invalid":true}`)
	got, err := ReplayLedger(append(slices.Clone(events), sibling), f.scope, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	want.Head = sibling.ID
	if !reflect.DeepEqual(got, want) {
		t.Fatal("sibling branch contaminated scope")
	}
}

func TestControllerFrozenPlanAndMissingEvidence(t *testing.T) {
	f := newControllerFixture(t, false)
	plan, scope, err := LoadCommittedPlan(t.Context(), f.journal, f.objects, f.scope.BranchID, f.scope.ExperimentID)
	gotJSON, gotErr := paired.CanonicalJSON(plan)
	wantJSON, wantErr := paired.CanonicalJSON(f.plan)
	if err != nil || gotErr != nil || wantErr != nil || string(gotJSON) != string(wantJSON) || scope != f.scope {
		t.Fatalf("load: %v", err)
	}
	plan.Limits.PipelineTokens++
	wrong, err := NewController(f.journal, f.objects, scope, plan, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Read(t.Context()); err == nil {
		t.Fatal("changed frozen plan accepted")
	}
	if _, _, err := LoadCommittedPlan(t.Context(), f.journal, f.objects, "other-branch", f.scope.ExperimentID); err == nil {
		t.Fatal("inherited experiment accepted")
	}
	lease := f.bound(t, "probe_0001", true)
	settlement := f.settlement(t, lease, "passed", true, 10)
	if err := f.controller.Complete(t.Context(), lease, settlement); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.workspace, "improvement", "paired", f.scope.ExperimentID, "objects", settlement.Evidence.Digest)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := f.controller.Read(t.Context()); err == nil {
		t.Fatal("missing evidence accepted")
	}
	if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
		t.Fatal("missing evidence admitted")
	}
}

func TestControllerInvalidUnboundProposalCanRetryOnce(t *testing.T) {
	f := newControllerFixture(t, true)
	proposal := f.bound(t, "proposal_procedure", true)
	if err := f.controller.Complete(t.Context(), proposal, f.settlement(t, proposal, "invalid_proposal", true, 20)); err != nil {
		t.Fatal(err)
	}
	retry := f.bound(t, "proposal_procedure", true)
	if err := f.controller.Complete(t.Context(), retry, f.settlement(t, retry, "passed", true, 20)); err != nil {
		t.Fatal(err)
	}
	if retry.Generation != 2 {
		t.Fatal("proposal generation not incremented")
	}
	if _, err := f.controller.Admit(t.Context(), "proposal_procedure"); err == nil {
		t.Fatal("proposal exceeded retry limit")
	}
}

func TestControllerLateConflictInvalidatesPublishedDecision(t *testing.T) {
	f := newControllerFixture(t, false)
	var first Lease
	for _, stage := range []string{"probe_0001", "probe_0002"} {
		lease := f.bound(t, stage, true)
		if first.StageID == "" {
			first = lease
		}
		if err := f.controller.Complete(t.Context(), lease, f.settlement(t, lease, "passed", true, 10)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := f.objects.PutJSON("experiment_report", map[string]string{"decision": "simulation_only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Finish(t.Context(), report, "simulation_only"); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Complete(t.Context(), first, f.settlement(t, first, "failed", true, 10)); !errors.Is(err, ErrCompletionConflict) {
		t.Fatalf("conflict: %v", err)
	}
	state := f.read(t)
	if state.Decision != "blocked" || state.Report == nil || state.Budget.SpentTokens != 20 {
		t.Fatalf("%+v", state)
	}
}

type failedObjects struct{ Objects }

func (failedObjects) PutJSON(string, any) (paired.ArtifactRef, error) {
	return paired.ArtifactRef{}, errors.New("artifact publication failed")
}

func TestControllerArtifactFailurePrecedesCreation(t *testing.T) {
	f := newControllerFixture(t, false)
	plan := f.plan
	plan.ExperimentID = "another"
	scope := f.scope
	scope.ExperimentID = "another"
	c, err := NewController(f.journal, failedObjects{f.objects}, scope, plan, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.journal.ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context()); err == nil {
		t.Fatal("artifact failure hidden")
	}
	after, err := f.journal.ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("event committed before artifact")
	}
}

func TestControllerReadOnlyReopenDoesNotWrite(t *testing.T) {
	f := newControllerFixture(t, false)
	before, err := f.journal.ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := team.OpenEventStoreReadOnly(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	objects, err := OpenObjects(f.workspace, f.scope.ExperimentID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = objects.Close() }()
	journal := StoreJournal{Store: store}
	plan, scope, err := LoadCommittedPlan(t.Context(), journal, objects, f.scope.BranchID, f.scope.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewController(journal, objects, scope, plan, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := f.journal.ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read-only projection mutated journal")
	}
}

func TestCommittedPlanRejectsUnpairedAndUnresolved(t *testing.T) {
	f := newControllerFixture(t, false)
	for _, kind := range []string{"model", "generation", "replicates", "cases", "budget", "policy", "unknown_variant"} {
		t.Run(kind, func(t *testing.T) {
			raw, err := paired.CanonicalJSON(f.plan)
			if err != nil {
				t.Fatal(err)
			}
			var plan paired.ExperimentPlan
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "model":
				plan.Models["executor"] = paired.ResolvedModel{}
			case "generation":
				model := plan.Models["executor"]
				model.Generation = nil
				plan.Models["executor"] = model
			case "replicates":
				plan.Slots[0].Replicate = 2
				plan.Slots[1].Replicate = 2
			case "cases":
				plan.Slots[1].Case = "unpaired"
			case "budget":
				plan.Limits.PipelineTokens = 100
			case "policy":
				plan.PolicyProjectionVersion = 9
			case "unknown_variant":
				plan.Slots = append(plan.Slots, paired.PairSlot{Variant: "extra", Case: "case_a", Replicate: 1})
			}
			if _, _, err := plannedStages(plan); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
}

func TestControllerReconcilePositiveNoDispatchProof(t *testing.T) {
	f := newControllerFixture(t, false)
	lease, err := f.controller.Admit(t.Context(), "probe_0001")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.objects.PutJSON("stage_evidence", map[string]string{"proof": "fake child journal confirms no provider admission"})
	if err != nil {
		t.Fatal(err)
	}
	result := Settlement{Evidence: ref, Outcome: "not_dispatched", UsageKnown: true, Stopped: true}
	executor := &fakeExecutor{reconcile: func(context.Context, StageRequest) (Reconciliation, error) {
		return Reconciliation{State: "completed", Settlement: result}, nil
	}}
	if err := f.controller.Reconcile(t.Context(), executor); err != nil {
		t.Fatal(err)
	}
	state := f.read(t)
	if state.Stages[lease.StageID].State != "completed" || state.Budget.ReservedTokens != 100 || state.Budget.SpentTokens != 0 {
		t.Fatalf("%+v", state)
	}
	if executor.calls.Load() != 0 {
		t.Fatal("proof recovery dispatched")
	}
}
