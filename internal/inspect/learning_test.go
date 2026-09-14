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
	empty := InspectLearning(t.Context(), t.TempDir(), "project", "team", "active")
	if empty.Status != "available" || empty.EmptyState != "no_recall_data" || empty.Exposures == nil || *empty.Exposures != 0 {
		t.Fatalf("missing database learning view = %#v", empty)
	}
	if empty.RequestedMode != "active" || empty.EffectiveMode != "off" || empty.UnavailableReason != "requested_mode_not_effective" {
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
	unknown := InspectLearning(t.Context(), workspace, "project", "team", "active")
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

	view := InspectLearning(t.Context(), workspace, "project", "team", "shadow")
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

func value(pointer *int64) int64 {
	if pointer == nil {
		return -1
	}
	return *pointer
}
