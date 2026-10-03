package inspect

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestInspectLearningDistinguishesEmptyFromUnknown(t *testing.T) {
	empty := InspectLearning(t.Context(), t.TempDir(), "project", "team", requestedPolicy("active"))
	if empty.Status != "available" || empty.EmptyState != "no_recall_data" || empty.Exposures == nil || *empty.Exposures != 0 {
		t.Fatalf("missing database learning view = %#v", empty)
	}
	if empty.RequestedMode != "active" || empty.EffectiveMode != "shadow" || empty.UnavailableReason != "requested_mode_not_effective" {
		t.Fatalf("missing database mode fallback = %#v", empty)
	}

	workspace := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE sentinel (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	unknown := InspectLearning(t.Context(), workspace, "project", "team", requestedPolicy("active"))
	if unknown.Status != "unknown" || unknown.Exposures != nil || unknown.UnavailableReason != "learning_policy_query_failed" {
		t.Fatalf("query failure learning view = %#v", unknown)
	}
}

func TestInspectLearningSeparatesSignalsAndOmitsPrivateMemory(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	shared := contextstore.ContextItem{ID: "shared", Kind: contextstore.ContextPattern, Content: "shared", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed}
	private := contextstore.ContextItem{ID: "private", Kind: contextstore.ContextPattern, Content: "private", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"}, Lifecycle: contextstore.LifecycleConfirmed}
	if err = repo.Append(t.Context(), shared, private); err != nil {
		t.Fatal(err)
	}
	for _, observation := range []contextstore.ExperienceObservation{
		{IdempotencyKey: "shared-signals", ContextItemID: shared.ID, PolicyVersion: "policy-v1", ProjectID: "project", TaskID: "task-1", ObservedAt: now, ExposureDelta: 5, ConsultedDelta: 4, AppliedDelta: 3, RejectedDelta: 2, VerifiedSupportDelta: 1},
		{IdempotencyKey: "private-signals", ContextItemID: private.ID, PolicyVersion: "policy-v1", ProjectID: "project", TaskID: "task-2", ObservedAt: now, ExposureDelta: 100, ConsultedDelta: 100, AppliedDelta: 100, VerifiedSupportDelta: 100},
	} {
		if _, err = repo.ApplyExperienceObservation(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	policy.PolicyVersion = "policy-v1"
	snapshot, err := json.Marshal(map[string]any{"id": "policy-v1", "revision_hash": "revision-1", "learning": policy})
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.SaveMemoryPolicyVersion(t.Context(), "policy-v1", snapshot, "revision-1", "active", now); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}

	view := InspectLearning(t.Context(), workspace, "project", "team", requestedPolicy("shadow"))
	if view.Status != "available" || view.EffectiveMode != "observe" || view.RequestedMode != "shadow" {
		t.Fatalf("learning mode view = %#v", view)
	}
	if value(view.Exposures) != 5 || value(view.Consulted) != 4 || value(view.Applied) != 3 || value(view.Rejected) != 2 || value(view.VerifiedSupport) != 1 {
		t.Fatalf("learning signals were conflated or included private memory: %#v", view)
	}
	if view.EligiblePromotions != nil {
		t.Fatalf("eligibility should remain unknown before explicit analysis: %#v", view)
	}
}

// TestInspectLearningEffectiveModeFollowsRuntimePrecedence pins the view to
// the mode a run uses: an adopted policy wins, and without one the team's
// requested mode applies, with active held at shadow until adoption.
func TestInspectLearningEffectiveModeFollowsRuntimePrecedence(t *testing.T) {
	cases := []struct {
		name          string
		store         string
		requested     string
		wantRequested string
		wantEffective string
		wantReason    string
	}{
		{name: "no store, no request", store: "none", requested: "", wantRequested: "unknown", wantEffective: "off"},
		{name: "no store, observe", store: "none", requested: "observe", wantRequested: "observe", wantEffective: "observe"},
		{name: "store without adoption, observe", store: "empty", requested: "observe", wantRequested: "observe", wantEffective: "observe"},
		{name: "store without adoption, off", store: "empty", requested: "off", wantRequested: "off", wantEffective: "off"},
		{name: "store without adoption, active", store: "empty", requested: "active", wantRequested: "active", wantEffective: "shadow", wantReason: "requested_mode_not_effective"},
		{name: "adopted policy wins", store: "adopted", requested: "shadow", wantRequested: "shadow", wantEffective: "observe", wantReason: "requested_mode_not_effective"},
		{name: "adopted policy matches", store: "adopted", requested: "observe", wantRequested: "observe", wantEffective: "observe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tc.store != "none" {
				repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if tc.store == "adopted" {
					policy := agent.DefaultMemoryLearningPolicy()
					policy.Mode = agent.MemoryLearningObserve
					snapshot, err := json.Marshal(map[string]any{"id": policy.PolicyVersion, "revision_hash": "revision-1", "learning": policy})
					if err != nil {
						t.Fatal(err)
					}
					if err = repo.SaveMemoryPolicyVersion(t.Context(), policy.PolicyVersion, snapshot, "revision-1", "active", time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
				if err = repo.Close(); err != nil {
					t.Fatal(err)
				}
			}
			view := InspectLearning(t.Context(), workspace, "project", "team", requestedPolicy(tc.requested))
			if view.Status != "available" || view.RequestedMode != tc.wantRequested || view.EffectiveMode != tc.wantEffective || view.UnavailableReason != tc.wantReason {
				t.Fatalf("view = status %q requested %q effective %q reason %q, want requested %q effective %q reason %q",
					view.Status, view.RequestedMode, view.EffectiveMode, view.UnavailableReason, tc.wantRequested, tc.wantEffective, tc.wantReason)
			}
			if view.PolicyVersion != agent.DefaultMemoryLearningPolicy().PolicyVersion {
				t.Fatalf("policy version = %q, want the default the runtime records", view.PolicyVersion)
			}
		})
	}
}

// TestInspectLearningCountsPromotedEvidenceOnce pins the totals after
// promotion: a persistent record inherits its session source's evidence, so
// counting the source too would report the same uses twice.
func TestInspectLearningCountsPromotedEvidenceOnce(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	session := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session-1"}
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	if err = repo.Append(t.Context(),
		contextstore.ContextItem{ID: "source", Kind: contextstore.ContextObservation, Content: "finding", Scope: session, Lifecycle: contextstore.LifecycleConfirmed},
		contextstore.ContextItem{ID: "promoted", Kind: contextstore.ContextPattern, Content: "pattern", Scope: shared, Lifecycle: contextstore.LifecycleConfirmed,
			Source: contextstore.SourceRef{Type: "shared_memory_candidate", Ref: contextstore.PromotedSessionRecordSourceRef}, Evidence: []contextstore.EvidenceRef{{ItemID: "source", Type: "context_item", Ref: "source"}}},
		contextstore.ContextItem{ID: "unrelated", Kind: contextstore.ContextPattern, Content: "other", Scope: shared, Lifecycle: contextstore.LifecycleConfirmed},
	); err != nil {
		t.Fatal(err)
	}
	policy := agent.DefaultMemoryLearningPolicy()
	for _, observation := range []contextstore.ExperienceObservation{
		{IdempotencyKey: "use", ContextItemID: "source", TaskID: "run-1/1", AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 0.5},
		{IdempotencyKey: "use\x1finherited\x1fpromoted", ContextItemID: "promoted", TaskID: "run-1/1", AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 0.5},
		{IdempotencyKey: "other", ContextItemID: "unrelated", TaskID: "run-2/1", AppliedDelta: 1},
	} {
		observation.PolicyVersion, observation.ProjectID, observation.ObservedAt = policy.PolicyVersion, "project", time.Now().UTC()
		if _, err = repo.ApplyExperienceObservation(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	view := InspectLearning(t.Context(), workspace, "project", "team", requestedPolicy("observe"))
	if value(view.Applied) != 2 || value(view.VerifiedSupport) != 1 {
		t.Fatalf("applied=%d verified=%d, want 2 and 1 with the promoted source counted once (view %#v)", value(view.Applied), value(view.VerifiedSupport), view)
	}
}

// requestedPolicy is a parsed team policy that sets only a mode, which is how
// a team definition requests learning.
func requestedPolicy(mode string) agent.MemoryLearningPolicy {
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningMode(mode)
	return policy
}

func value(pointer *int64) int64 {
	if pointer == nil {
		return -1
	}
	return *pointer
}
