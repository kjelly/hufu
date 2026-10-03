package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestInheritExperience(t *testing.T) {
	source := contextstore.ExperienceObservation{IdempotencyKey: "use-1", ContextItemID: "session-a", TaskID: "run-1/1", AppliedDelta: 1}
	other := contextstore.ExperienceObservation{IdempotencyKey: "use-2", ContextItemID: "session-b", TaskID: "run-1/2", ConsultedDelta: 1}
	cases := []struct {
		name    string
		lineage []contextstore.ExperienceLineage
		want    []contextstore.ExperienceObservation
	}{
		{name: "no lineage", want: []contextstore.ExperienceObservation{source, other}},
		{
			name:    "one link copies only its source",
			lineage: []contextstore.ExperienceLineage{{SourceID: "session-a", TargetID: "persistent-a"}},
			want: []contextstore.ExperienceObservation{source, other,
				{IdempotencyKey: "use-1\x1finherited\x1fpersistent-a", ContextItemID: "persistent-a", TaskID: "run-1/1", AppliedDelta: 1}},
		},
		{
			name: "two targets each inherit",
			lineage: []contextstore.ExperienceLineage{
				{SourceID: "session-a", TargetID: "persistent-a"},
				{SourceID: "session-a", TargetID: "persistent-b"},
			},
			want: []contextstore.ExperienceObservation{source, other,
				{IdempotencyKey: "use-1\x1finherited\x1fpersistent-a", ContextItemID: "persistent-a", TaskID: "run-1/1", AppliedDelta: 1},
				{IdempotencyKey: "use-1\x1finherited\x1fpersistent-b", ContextItemID: "persistent-b", TaskID: "run-1/1", AppliedDelta: 1}},
		},
		{
			name: "a copy is not copied again",
			lineage: []contextstore.ExperienceLineage{
				{SourceID: "session-a", TargetID: "persistent-a"},
				{SourceID: "persistent-a", TargetID: "persistent-z"},
			},
			want: []contextstore.ExperienceObservation{source, other,
				{IdempotencyKey: "use-1\x1finherited\x1fpersistent-a", ContextItemID: "persistent-a", TaskID: "run-1/1", AppliedDelta: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InheritExperience([]contextstore.ExperienceObservation{source, other}, tc.lineage)
			if len(got) != len(tc.want) {
				t.Fatalf("observations = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("observation %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPromotedRecordInheritsSessionEvidence pins the learning loop across
// promotion: a session record applied by a verified task keeps that evidence
// when an accepted run promotes it, later evidence on the session record
// still reaches the promoted record, and a rebuild from the event log
// reproduces the same aggregate.
func TestPromotedRecordInheritsSessionEvidence(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := freshKnowledgeCoordinator(workspace, repo, agent.MemoryLearningObserve, "run-1", "session-1")
	c.eventStore = store
	c.emittedTaskTransitions = make(map[string]bool)

	c.reduceTaskResultToSharedMemory(ctx, TaskResultMemoryInput{TodoID: "1", Attempt: 1, Result: &TaskResult{
		Findings: []Finding{{Summary: "Trim the closing quote before redacting", Detail: "lineage-marker"}},
	}})
	sessionID := lineageTestRecordID(t, repo, c.contextScope(), false)
	policy := c.session.Config.MemoryLearning
	emitLineageTestEvent(t, c, "memory_usage_recorded", "usage-1", "run-1/2", map[string]any{"context_item_id": sessionID, "disposition": MemoryUseApplied, "confidence": 0.9})
	emitLineageTestEvent(t, c, "memory_outcome_recorded", "outcome-1", "run-1/2", map[string]any{"context_item_id": sessionID, "signal": "verification_passed", "direction": "positive", "effective_weight": 0.5})

	c.autoExtractCanonicalLTM(ctx, "run-1")
	if err := c.confirmSharedMemoryCandidates(ctx, &EvidenceManifest{RunID: "run-1", Status: "accepted", ManifestHash: "manifest-1"}); err != nil {
		t.Fatal(err)
	}
	persistentID := lineageTestRecordID(t, repo, c.contextScope(), true)
	inherited, err := repo.ExperienceAggregate(ctx, persistentID, policy.PolicyVersion)
	if err != nil {
		t.Fatalf("promoted record has no aggregate: %v", err)
	}
	if inherited.AppliedCount != 1 || inherited.VerifiedSupportCount != 1 || inherited.IndependentTaskCount != 1 || inherited.PositiveWeight != 0.5 {
		t.Fatalf("promoted aggregate = %+v, want the session record's applied and verified evidence", inherited)
	}

	emitLineageTestEvent(t, c, "memory_usage_recorded", "usage-2", "run-1/3", map[string]any{"context_item_id": sessionID, "disposition": MemoryUseApplied, "confidence": 0.5})
	live, err := repo.ExperienceAggregate(ctx, persistentID, policy.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if live.AppliedCount != 2 || live.IndependentTaskCount != 2 {
		t.Fatalf("later session evidence did not reach the promoted record: %+v", live)
	}

	if err := c.RebuildExperienceAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := repo.ExperienceAggregate(ctx, persistentID, policy.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.AppliedCount != live.AppliedCount || rebuilt.VerifiedSupportCount != live.VerifiedSupportCount ||
		rebuilt.IndependentTaskCount != live.IndependentTaskCount || rebuilt.PositiveWeight != live.PositiveWeight {
		t.Fatalf("rebuilt aggregate = %+v, want the live one %+v", rebuilt, live)
	}
}

func lineageTestRecordID(t *testing.T, repo *contextstore.SQLiteRepository, scope contextstore.Scope, persistent bool) string {
	t.Helper()
	if persistent {
		scope.SessionID = ""
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: scope, Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		promoted := item.Source.Ref == contextstore.PromotedSessionRecordSourceRef && item.Scope.SessionID == ""
		if (persistent && promoted) || (!persistent && item.Kind == contextstore.ContextObservation && item.Scope.SessionID != "") {
			return item.ID
		}
	}
	t.Fatalf("no %s record among %+v", map[bool]string{true: "promoted persistent", false: "session observation"}[persistent], items)
	return ""
}

// emitLineageTestEvent records a memory event the way the runtime does: a
// durable append followed by the projection reducer.
func emitLineageTestEvent(t *testing.T, c *Coordinator, eventType, key, occurrence string, fields map[string]any) {
	t.Helper()
	fields["schema_version"] = memoryEventSchemaVersion
	fields["policy_version"] = c.session.Config.MemoryLearning.PolicyVersion
	fields["project_id"] = c.contextScope().ProjectID
	fields["occurrence_id"] = occurrence
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	event := RunEvent{Type: eventType, Actor: "worker", TaskID: "2", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), IdempotencyKey: key, Payload: raw}
	emitted, err := c.emitEventOnce(key, event)
	if err != nil || !emitted {
		t.Fatalf("emit %s: emitted=%v err=%v", eventType, emitted, err)
	}
	c.reduceMemoryEvent(event)
}
