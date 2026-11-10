package team

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestHistoricalResultGrantCycleProjection(t *testing.T) {
	grant := historicalGrantEventFixture(t)
	grant.Hash = strings.Repeat("f", 64)
	resume := RunEvent{Type: string(EventRunStarted), RunID: "resume-invocation"}
	next := RunEvent{Type: string(EventWorkflowCycleStarted), RunID: "next-cycle", Payload: []byte(`{"request":"new request","task_id_floor":1}`)}
	tests := []struct {
		name   string
		events []RunEvent
		active bool
		valid  bool
	}{
		{"initial", []RunEvent{grant}, true, true},
		{"resume_retains_original_cycle", []RunEvent{grant, resume}, true, true},
		{"followup_clears_grants", []RunEvent{grant, next, resume}, false, true},
		{"duplicate_grant", []RunEvent{grant, func() RunEvent {
			e := grant
			e.ID, e.IdempotencyKey = "another-grant-event", "another-key"
			return e
		}()}, false, false},
		{"late_grant", []RunEvent{{Type: string(EventTaskCreated)}, grant}, false, false},
		{"wrong_initial_cycle", []RunEvent{resume, grant}, false, false},
		{"old_grant_in_new_cycle", []RunEvent{next, grant}, false, false},
		{"invalid_envelope", []RunEvent{{Type: grant.Type, Payload: []byte(`{}`)}}, false, false},
		{"missing_durable_hash", []RunEvent{func() RunEvent { e := grant; e.Hash = ""; return e }()}, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref, payload, err := historicalResultGrantProjection(test.events)
			if (err == nil) != test.valid {
				t.Fatalf("projection validity = %v, error %v", err == nil, err)
			}
			session := ReduceToSessionData(test.events)
			if !test.valid {
				if !session.RecoveryRequired || session.HistoricalResultGrants != nil {
					t.Fatal("invalid lineage established a grant or failed to require recovery")
				}
				return
			}
			if (ref != nil) != test.active || (payload != nil) != test.active || !reflect.DeepEqual(session.HistoricalResultGrants, ref) {
				t.Fatalf("active grant projection = %+v, payload %+v, session %+v", ref, payload, session.HistoricalResultGrants)
			}
			if ref != nil && (ref.CycleID != grant.RunID || ref.EventID != grant.ID || ref.EventHash != grant.Hash) {
				t.Fatal("resume changed the original grant identity")
			}
		})
	}
}

// This fixture writes the real policy and grant/index transaction; it leaves
// the checkpoint absent to exercise append-before-checkpoint recovery.
func historicalGrantReplayFixture(t *testing.T) (*Coordinator, *EventStore, *SessionTree, RunEvent) {
	t.Helper()
	c, rows, _ := historicalGrantPreparationFixture(t)
	prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + (*rows)[0].ResultID})
	if err != nil {
		t.Fatal(err)
	}
	return historicalGrantPreparedReplayFixture(t, c, prepared)
}

func historicalGrantPreparedReplayFixture(t *testing.T, c *Coordinator, prepared *PreparedHistoricalResultGrants) (*Coordinator, *EventStore, *SessionTree, RunEvent) {
	t.Helper()
	payload, err := prepared.envelope("consumer-session", "consumer-cycle")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(c.session.Workspace, payload.CycleID, payload.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.SetSessionData(NewSession())
	store.SetBranchID("main")
	policyJSON, err := json.Marshal(c.ExecutionPolicySnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", Payload: policyJSON}); err != nil {
		t.Fatal(err)
	}
	grantJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.AppendPersisted(RunEvent{Type: string(EventHistoricalResultsGranted), Actor: "operator", Payload: grantJSON, IdempotencyKey: "grants:" + payload.CycleID})
	if err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	return c, store, tree, durable
}

func TestHistoricalResultGrantCheckpointRecovery(t *testing.T) {
	c, store, tree, grant := historicalGrantReplayFixture(t)
	c.checkCanonicalProjectionShadow(tree, "main")
	got := c.SessionData()
	if got.RecoveryRequired || got.HistoricalResultGrants == nil || got.HistoricalResultGrants.EventID != grant.ID {
		t.Fatalf("canonical grant was not restored after append-only crash: %+v", got)
	}
	persisted, exists, err := LoadSessionReadOnly(c.session.Workspace)
	if err != nil || !exists || !reflect.DeepEqual(persisted.HistoricalResultGrants, got.HistoricalResultGrants) {
		t.Fatalf("grant checkpoint = %+v, exists %v, error %v", persisted, exists, err)
	}
	// The public snapshot must not expose a mutable pointer into live state.
	got.HistoricalResultGrants.EventID = "forged"
	if c.SessionData().HistoricalResultGrants.EventID != grant.ID {
		t.Fatal("snapshot mutation changed the live grant reference")
	}
	if err := CompareCanonicalProjection(c.SessionData(), mustReadEvents(t, store)); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalResultGrantIndexRequiredForRecovery(t *testing.T) {
	c, store, tree, _ := historicalGrantReplayFixture(t)
	if _, err := store.store.DB().ExecContext(t.Context(), `DELETE FROM historical_grant_cycles`); err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, store)
	if _, err := store.validateHistoricalResultGrantProjection(t.Context(), before); !errors.Is(err, runtimestore.ErrHistoricalGrantSource) {
		t.Fatalf("missing index was accepted: %v", err)
	}
	c.checkCanonicalProjectionShadow(tree, "main")
	if !c.SessionData().RecoveryRequired || c.SessionData().HistoricalResultGrants != nil {
		t.Fatal("missing index did not block recovery")
	}
	if err := RebuildSessionForBranch(c.session.Workspace, tree, store, "main"); !errors.Is(err, runtimestore.ErrHistoricalGrantSource) {
		t.Fatalf("checkout rebuilt a missing grant index: %v", err)
	}
	if after := mustReadEvents(t, store); len(after) != len(before) {
		t.Fatal("recovery manufactured a replacement grant event")
	}
}

func TestHistoricalResultGrantForkVisibility(t *testing.T) {
	c, store, tree, grant := historicalGrantReplayFixture(t)
	events := mustReadEvents(t, store)
	before, err := tree.CreateBranch("before-grant", events[0].ID, store)
	if err != nil {
		t.Fatal(err)
	}
	after, err := tree.CreateBranch("after-grant", grant.ID, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		branch string
		active bool
	}{{before.ID, false}, {after.ID, true}, {"main", true}} {
		t.Run(test.branch, func(t *testing.T) {
			if err := RebuildSessionForBranch(c.session.Workspace, tree, store, test.branch); err != nil {
				t.Fatal(err)
			}
			session := LoadSession(c.session.Workspace)
			if (session.HistoricalResultGrants != nil) != test.active {
				t.Fatalf("fork grant = %+v", session.HistoricalResultGrants)
			}
			if test.active && (session.HistoricalResultGrants.BranchID != "main" || session.HistoricalResultGrants.EventID != grant.ID) {
				t.Fatal("fork replaced the ancestor-owned grant identity")
			}
		})
	}
	if _, found, err := runtimestore.HistoricalGrantCycle(t.Context(), store.store.DB(), after.ID, grant.RunID); err != nil || found {
		t.Fatalf("fork fabricated a child grant index: found %v, error %v", found, err)
	}
}

func TestHistoricalResultGrantProjectionCannotBeForged(t *testing.T) {
	c, store, tree, _ := historicalGrantReplayFixture(t)
	canonical := ReduceToSessionData(mustReadEvents(t, store))
	for _, field := range []string{"event", "hash", "branch", "cycle"} {
		t.Run(field, func(t *testing.T) {
			live := *canonical
			live.HistoricalResultGrants = cloneHistoricalGrantCycleRef(canonical.HistoricalResultGrants)
			switch field {
			case "event":
				live.HistoricalResultGrants.EventID = "forged"
			case "hash":
				live.HistoricalResultGrants.EventHash = strings.Repeat("0", 64)
			case "branch":
				live.HistoricalResultGrants.BranchID = "sibling"
			case "cycle":
				live.HistoricalResultGrants.CycleID = "other-cycle"
			}
			if err := CompareCanonicalProjection(&live, mustReadEvents(t, store)); err == nil {
				t.Fatal("shadow comparison accepted a forged grant reference")
			}
			c.SetSessionData(&live)
			c.checkCanonicalProjectionShadow(tree, "main")
			if !c.SessionData().RecoveryRequired {
				t.Fatal("startup silently accepted or repaired a forged grant")
			}
		})
	}
}

func TestHistoricalResultGrantCheckpointCannotCreateAuthority(t *testing.T) {
	c := newTestCoordinator(t)
	c.initEventStore()
	c.SetSessionData(&SessionData{HistoricalResultGrants: &runtimestore.HistoricalGrantCycleRef{
		BranchID: "main", CycleID: "fake-cycle", EventID: "fake-event", EventHash: strings.Repeat("f", 64),
	}})
	c.checkCanonicalProjectionShadow(NewSessionTree(), "main")
	if !c.SessionData().RecoveryRequired {
		t.Fatal("checkpoint-only grant was accepted on an empty journal")
	}
}

func TestHistoricalResultGrantActiveBranchRefresh(t *testing.T) {
	c, store, tree, grant := historicalGrantReplayFixture(t)
	if err := RefreshActiveBranchProjection(c.session.Workspace, tree, store); err != nil {
		t.Fatal(err)
	}
	if got := LoadSession(c.session.Workspace).HistoricalResultGrants; got == nil || got.EventID != grant.ID {
		t.Fatalf("refresh lost canonical grant: %+v", got)
	}
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventWorkflowCycleStarted), RunID: "next-cycle", Actor: "coordinator",
		Payload: []byte(`{"request":"next request","task_id_floor":0}`)}); err != nil {
		t.Fatal(err)
	}
	if err := RefreshActiveBranchProjection(c.session.Workspace, tree, store); err != nil {
		t.Fatal(err)
	}
	if LoadSession(c.session.Workspace).HistoricalResultGrants != nil {
		t.Fatal("refresh carried a previous cycle's grants into a fresh request")
	}
	if ref, found, err := runtimestore.HistoricalGrantCycle(t.Context(), store.store.DB(), "main", grant.RunID); err != nil || !found || ref.EventID != grant.ID {
		t.Fatal("fresh request destroyed the historical grant index")
	}
}

func TestHistoricalResultGrantCanonicalScopeAndPolicy(t *testing.T) {
	for _, failure := range []string{"workspace", "policy", "policy_after_grant", "missing_policy"} {
		t.Run(failure, func(t *testing.T) {
			c, original, _, grant := historicalGrantReplayFixture(t)
			policy := mustReadEvents(t, original)[0]
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, grant.RunID, grant.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			grant = changeHistoricalGrantPayload(t, grant, func(payload *HistoricalResultGrantsPayload) {
				payload.Workspace = workspace
				if failure == "workspace" {
					payload.Workspace = c.session.Workspace
				}
				if failure == "policy" {
					payload.PolicyFingerprint = strings.Repeat("0", 64)
				}
			})
			events := []RunEvent{policy, grant}
			switch failure {
			case "policy_after_grant":
				events = []RunEvent{grant, policy}
			case "missing_policy":
				events = []RunEvent{grant}
			}
			for _, event := range events {
				if _, err := store.AppendPersisted(event); err != nil {
					t.Fatal(err)
				}
			}
			if payload, err := store.validateHistoricalResultGrantProjection(t.Context(), mustReadEvents(t, store)); err == nil || payload != nil {
				t.Fatal("grant accepted a different scope, absent policy or a policy admitted too late")
			}
		})
	}
}
