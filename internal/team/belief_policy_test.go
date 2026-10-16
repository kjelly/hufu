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
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/belief"
)

func beliefTestPolicy() belief.Policy {
	return belief.Policy{SchemaVersion: 1, Version: "synthetic-v1", Algorithm: belief.PolicyAlgorithm, Mode: "advisory", Choices: []belief.PolicyChoice{{HypothesisID: "h1", CandidateID: "check-a"}, {HypothesisID: "h2", CandidateID: "check-b"}}}
}

func beliefTestCatalog(t testing.TB, actor string) *ActionCatalogSnapshot {
	t.Helper()
	catalog := &ActionCatalogSnapshot{Version: 1}
	for _, id := range []string{"check-a", "check-b"} {
		entry := ActionCatalogEntry{ID: id, Description: "Synthetic advisory fixture", Capability: "synthetic-checks", Type: "check", Agent: actor, SideEffect: SideEffectNone, Recovery: RecoveryRetry, InputSchema: RunInputSchema{Type: "object"}, Discover: []string{actor}, Propose: []string{actor}, MaxInvocations: 1}
		var err error
		entry.Hash, err = hashActionCatalogEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		catalog.Entries = append(catalog.Entries, entry)
	}
	if err := catalog.rehash(); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func newBeliefPolicyFixture(t testing.TB) beliefFixture {
	t.Helper()
	f := newBeliefFixture(t)
	policy, err := CompileBeliefPolicy(f.model, beliefTestPolicy(), beliefTestCatalog(t, "worker"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.journal.BindRuntimeModel(t.Context(), f.model, "comparison", &policy); err != nil {
		t.Fatal(err)
	}
	if err := f.journal.OpenEpoch(t.Context(), f.epoch); err != nil {
		t.Fatal(err)
	}
	f.admit(t)
	return f
}

func (f beliefFixture) policySnapshot(t *testing.T) string {
	t.Helper()
	events, err := f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	input, err := policyContext(t.Context(), events, f.artifacts, f.scope.RunID, f.epoch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return beliefPolicySnapshotDigest(input.epoch, input.actor)
}

func (f beliefFixture) policyViews(t *testing.T) []BeliefSuggestionView {
	t.Helper()
	events, err := f.journal.events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	views, err := ProjectBeliefSuggestions(t.Context(), events, f.artifacts, f.scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return views
}

func TestBeliefPolicyCatalogAndConfigurationBoundary(t *testing.T) {
	f := newBeliefFixture(t)
	for _, mutate := range []func(*belief.Policy, *ActionCatalogSnapshot){
		func(p *belief.Policy, _ *ActionCatalogSnapshot) { p.Mode = "active" },
		func(p *belief.Policy, _ *ActionCatalogSnapshot) { p.Choices[1].HypothesisID = "unknown" },
		func(p *belief.Policy, _ *ActionCatalogSnapshot) { p.Choices[1].CandidateID = "unknown" },
		func(_ *belief.Policy, c *ActionCatalogSnapshot) { c.Entries[1].SideEffect = SideEffectWorkspaceWrite },
		func(_ *belief.Policy, c *ActionCatalogSnapshot) { c.Entries[1].Discover = nil },
	} {
		policy, catalog := beliefTestPolicy(), beliefTestCatalog(t, "worker")
		mutate(&policy, catalog)
		if _, err := CompileBeliefPolicy(f.model, policy, catalog); err == nil {
			t.Fatal("accepted a policy outside existing catalog grants")
		}
	}
	dir := t.TempDir()
	writeBeliefModel(t, dir)
	policyBytes, err := json.Marshal(beliefTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), policyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	session := &TeamSession{Dir: dir, Config: agent.TeamConfig{Belief: &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison", PolicyFile: "policy.json"}}, ActionCatalog: beliefTestCatalog(t, "worker")}
	if err := compileTeamBelief(session); err != nil {
		t.Fatal(err)
	}
	first := executionPolicyBelief(session).PolicyDigest
	if first == "" {
		t.Fatal("policy not frozen in execution snapshot")
	}
	policy := beliefTestPolicy()
	policy.Version = "synthetic-v2"
	policyBytes, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), policyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compileTeamBelief(session); err != nil {
		t.Fatal(err)
	}
	if first == executionPolicyBelief(session).PolicyDigest {
		t.Fatal("policy drift did not change execution snapshot")
	}
	for _, name := range []string{"../policy.json", "/policy.json"} {
		session.Config.Belief.PolicyFile = name
		if err := compileTeamBelief(session); err == nil {
			t.Fatal("accepted policy path outside team")
		}
	}
	outside := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(outside, policyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escaping.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readBeliefPolicyFile(dir, "escaping.json"); err == nil {
		t.Fatal("accepted escaping policy symlink")
	}
}

func TestBeliefPolicyDurableGenerationAndRetraction(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	snapshot := f.policySnapshot(t)
	first, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0)
	if err != nil || first.SelectedCandidateID != "check-b" || first.SelectedHypothesisID != "h2" || first.DecisionGeneration != 1 || first.StateDigest != f.state(t).State.Digest || first.Synthetic == nil || !*first.Synthetic {
		t.Fatalf("first=%+v error=%v", first, err)
	}
	for range 100 {
		replayed, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0)
		if err != nil || !reflect.DeepEqual(first, replayed) {
			t.Fatal("retry changed durable advice")
		}
	}
	if err := f.journal.CloseEpoch(t.Context(), f.epoch.ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if f.policySnapshot(t) != snapshot || len(f.policyViews(t)) != 1 {
		t.Fatal("closure changed advisory generation")
	}
	observation := f.state(t).Observations[0]
	if err := f.journal.Retract(t.Context(), f.epoch.ID, observation.ID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 1); err == nil {
		t.Fatal("accepted stale belief snapshot")
	}
	nextSnapshot := f.policySnapshot(t)
	if _, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, nextSnapshot, 0); err == nil {
		t.Fatal("accepted stale generation")
	}
	second, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, nextSnapshot, 1)
	if err != nil || second.DecisionGeneration != 2 || second.StateDigest == first.StateDigest || second.SnapshotDigest == first.SnapshotDigest {
		t.Fatalf("second=%+v error=%v", second, err)
	}
	views := f.policyViews(t)
	if len(views) != 2 || views[0].Validation != "verified" || views[1].Validation != "verified" {
		t.Fatalf("history=%+v", views)
	}
	// A different policy cannot silently reuse the model-bound idempotency key.
	policy, err := CompileBeliefPolicy(f.model, beliefTestPolicy(), beliefTestCatalog(t, "worker"))
	if err != nil {
		t.Fatal(err)
	}
	policy.Policy.Version = "other"
	if err := f.journal.BindRuntimeModel(t.Context(), f.model, "comparison", &policy); err == nil {
		t.Fatal("policy replacement accepted inside an existing run")
	}
}

type beliefPolicyFaultJournal struct {
	ConditionalEventJournal
	before func()
	after  bool
}

func (j *beliefPolicyFaultJournal) AppendAtHead(ctx context.Context, event RunEvent, head string) (RunEvent, error) {
	if j.before != nil {
		callback := j.before
		j.before = nil
		callback()
	}
	result, err := j.ConditionalEventJournal.AppendAtHead(ctx, event, head)
	if err == nil && j.after {
		j.after = false
		return RunEvent{}, errors.New("injected uncertain advisory commit")
	}
	return result, err
}

func TestBeliefPolicyLateAppendAndUncertainCommit(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	oldSnapshot := f.policySnapshot(t)
	adapter := &beliefPolicyFaultJournal{ConditionalEventJournal: eventStoreJournal{f.store}, before: func() {
		if err := f.journal.Retract(t.Context(), f.epoch.ID, f.state(t).Observations[0].ID, f.source.ID); err != nil {
			t.Fatal(err)
		}
	}}
	writer, err := NewBeliefJournal(adapter, f.artifacts, f.tree, "main", f.scope.RunID, f.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.SuggestPolicy(t.Context(), f.epoch.ID, oldSnapshot, 0); !errors.Is(err, ErrEventHeadChanged) {
		t.Fatalf("late computation persisted: %v", err)
	}
	if len(f.policyViews(t)) != 0 {
		t.Fatal("stale computation left a suggestion")
	}
	adapter.after = true
	snapshot := f.policySnapshot(t)
	if _, err := writer.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0); err == nil {
		t.Fatal("fault did not fire")
	}
	retried, err := writer.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0)
	if err != nil || retried.DecisionGeneration != 1 || len(f.policyViews(t)) != 1 {
		t.Fatalf("uncertain commit duplicated: %+v %v", retried, err)
	}
}

func TestBeliefPolicyConcurrentWriters(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	snapshot := f.policySnapshot(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		writer, err := NewBeliefJournal(eventStoreJournal{f.store}, f.artifacts, f.tree, "main", f.scope.RunID, f.scope.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			<-start
			_, err := writer.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrEventHeadChanged) {
			t.Fatal(err)
		}
	}
	if _, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if len(f.policyViews(t)) != 1 {
		t.Fatal("concurrent writers duplicated generation")
	}
}

func TestBeliefPolicyReplayRejectsForgedAdvice(t *testing.T) {
	f := newBeliefPolicyFixture(t)
	if _, err := f.journal.SuggestPolicy(t.Context(), f.epoch.ID, f.policySnapshot(t), 0); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(events, func(e RunEvent) bool { return e.Type == EventBeliefPolicySuggested })
	for _, mutate := range []func(*BeliefPolicySuggestion){
		func(r *BeliefPolicySuggestion) { r.SelectedCandidateID = "check-a" },
		func(r *BeliefPolicySuggestion) { r.SelectedHypothesisID = "h1" },
		func(r *BeliefPolicySuggestion) { r.DecisionGeneration++ },
		func(r *BeliefPolicySuggestion) { r.SnapshotDigest = strings.Repeat("0", 64) },
		func(r *BeliefPolicySuggestion) { r.SourceCursorID = f.source.ID },
		func(r *BeliefPolicySuggestion) { r.SourceActor = "intruder" },
	} {
		copyEvents := slices.Clone(events)
		var record BeliefPolicySuggestion
		if err := json.Unmarshal(events[index].Payload, &record); err != nil {
			t.Fatal(err)
		}
		mutate(&record)
		copyEvents[index].Payload, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ProjectBeliefSuggestions(t.Context(), copyEvents, f.artifacts, f.scope.RunID); err == nil {
			t.Fatalf("accepted forged suggestion: %+v", record)
		}
	}
}

func TestBeliefPolicyAppendCapacityBoundaries(t *testing.T) {
	// One append at the last available slot is replayable. Exhausting either
	// the run history or epoch generation must refuse it before journal I/O.
	for _, test := range []struct {
		records    int
		generation uint64
		wantError  bool
	}{
		{records: 0, generation: 0},
		{records: 4095, generation: 1023},
		{records: 4096, generation: 0, wantError: true},
		{records: 0, generation: 1024, wantError: true},
	} {
		if err := validateBeliefAdviceCapacity(test.records, test.generation); (err != nil) != test.wantError {
			t.Fatalf("records=%d generation=%d error=%v", test.records, test.generation, err)
		}
	}
}
