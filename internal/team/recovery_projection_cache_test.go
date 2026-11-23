package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRecoveryProjectionCacheBindsActualPayload(t *testing.T) {
	t.Parallel()
	var cache recoveryProjectionCache
	events := []RunEvent{{ID: "source", Hash: strings.Repeat("a", 64), Payload: []byte(`{"value":1}`)}}
	calls := 0
	compute := func() (any, error) { calls++; return map[string]int{"computed": calls}, nil }
	for range 2 {
		if _, err := cache.resolve(t.Context(), "projection", events, nil, compute); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("an identical pure projection was recomputed")
	}
	// Even a whitespace-only payload change with the same claimed hash is a
	// different cache input; journal verification remains an independent gate.
	events[0].Payload = []byte(`{ "value":1 }`)
	if _, err := cache.resolve(t.Context(), "projection", events, nil, compute); err != nil || calls != 2 {
		t.Fatalf("actual payload bytes did not invalidate the cache: calls=%d err=%v", calls, err)
	}
	if _, err := cache.resolve(t.Context(), "other_projection", events, nil, compute); err != nil || calls != 3 {
		t.Fatalf("projection namespaces collided: calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.resolve(ctx, "projection", events, nil, compute); !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatal("a cache hit ignored cancellation")
	}
}

func TestRecoveryProjectionCachePreservesEmptyPolicyBindings(t *testing.T) {
	t.Parallel()
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 1, 0)
	snapshot := c.ExecutionPolicySnapshot()
	if snapshot == nil {
		t.Fatal("fixture has no policy")
	}
	snapshot.ResultContracts = nil
	var err error
	snapshot.ConfigurationHash, err = executionPolicyConfigurationHash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(snapshot, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	if err != nil {
		t.Fatal(err)
	}
	events := []RunEvent{{SchemaVersion: eventStoreSchemaVersion, ID: "empty-contract-policy", Type: string(EventExecutionPolicySnapshot), Payload: payload}}
	uncached, err := executionPolicySnapshotFromEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cached, err := c.cachedExecutionPolicyProjection(t.Context(), events)
		if err != nil || !reflect.DeepEqual(uncached, cached) {
			t.Fatalf("cache changed the canonical policy representation: %v", err)
		}
		if err := validateExecutionPolicySnapshot(cached); err != nil {
			t.Fatalf("cache invalidated the original policy hash: %v", err)
		}
	}
}

func TestRecoveryObservationProjectionRejectsChangedHistory(t *testing.T) {
	t.Parallel()
	response := strings.Replace(researchCollectionResponse(t, 2), `"structured_payload":{`, `"structured_payload":{"locations":["dir/soruce.go:7"],`, 1)
	c, source, id := contractCollectionFixture(t, response, SideEffectNone, contractCollectionFixtureOptions{
		Accepted: true, ValidAccepted: true, ArtifactKeys: true, FrozenPaths: []string{"dir/source.go"},
	})
	ctx := contractCollectionContext(t, id)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "cached-recovery", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	index := -1
	for i, event := range child.lineage {
		if event.Type == string(EventContractRecovery) && event.BranchID == branch {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("recovery event missing")
	}
	event, preceding := child.lineage[index], child.lineage[:index]
	first, err := c.observationRecoveryPlan(t.Context(), event, preceding)
	if err != nil {
		t.Fatal(err)
	}
	want := first.Tasks[0].Reason
	first.Tasks[0].Reason = "caller mutation"
	fresh, err := c.observationRecoveryPlan(t.Context(), event, preceding)
	if err != nil || fresh.Tasks[0].Reason != want {
		t.Fatal("caller mutation changed the cached recovery authority")
	}
	for _, scenario := range []string{"recovery_payload", "source_payload"} {
		t.Run(scenario, func(t *testing.T) {
			candidate, prefix := cloneRunEvent(event), cloneRunEvents(preceding)
			if scenario == "recovery_payload" {
				changed, err := decodeContractRecovery(candidate)
				if err != nil {
					t.Fatal(err)
				}
				changed.SourceHash = strings.Repeat("a", 64)
				changed.SHA256, _ = recoveryPlanHash(*changed)
				candidate.Payload = mustProjectionJSON(t, changed)
			} else {
				found := false
				for i := len(prefix) - 1; i >= 0; i-- {
					if prefix[i].Type != string(EventTaskCompleted) || prefix[i].TaskID != id {
						continue
					}
					var object map[string]any
					if err := json.Unmarshal(prefix[i].Payload, &object); err != nil {
						t.Fatal(err)
					}
					object["typed_result"].(map[string]any)["summary"] = "changed source claims"
					prefix[i].Payload = mustProjectionJSON(t, object)
					found = true
					break
				}
				if !found {
					t.Fatal("accepted source event missing")
				}
			}
			if _, err := c.observationRecoveryPlan(t.Context(), candidate, prefix); err == nil {
				t.Fatal("changed bytes reused the previously validated history")
			}
		})
	}
	policy, err := c.cachedExecutionPolicyProjection(t.Context(), preceding)
	if err != nil || policy == nil || len(policy.Backends) == 0 {
		t.Fatalf("policy projection: %v", err)
	}
	uncached, err := executionPolicySnapshotFromEvents(preceding)
	if err != nil || !reflect.DeepEqual(policy, uncached) {
		t.Fatal("cache changed the recovered policy representation")
	}
	backend := policy.Backends[0].Backend
	policy.Backends[0].Backend = "caller mutation"
	policy, err = c.cachedExecutionPolicyProjection(t.Context(), preceding)
	if err != nil || policy.Backends[0].Backend != backend {
		t.Fatal("caller mutation changed the cached policy")
	}
	invalid := append(cloneRunEvents(preceding), RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: "invalid-policy", Type: string(EventExecutionPolicySnapshot), Payload: []byte(`{}`)})
	if _, err := c.cachedExecutionPolicyProjection(t.Context(), invalid); err == nil {
		t.Fatal("a later policy event reused the old policy projection")
	}
}
