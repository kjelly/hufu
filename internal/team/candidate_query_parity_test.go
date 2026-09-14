package team

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestCandidateSettlementTypedQueriesPreserveCallerResults(t *testing.T) {
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	created := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	shared := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	session := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session"}
	worker := contextstore.Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"}
	workerRequest := contextstore.Scope{ProjectID: "project", TeamID: "team", BranchID: "main", AgentID: "worker"}
	item := func(id string, scope contextstore.Scope, lifecycle contextstore.ContextLifecycle, runID, sourceType string, priority contextstore.Priority, metadata map[string]string) contextstore.ContextItem {
		if metadata == nil {
			metadata = make(map[string]string)
		}
		metadata["run_id"] = runID
		return contextstore.ContextItem{
			ID: id, Kind: contextstore.ContextObservation, Content: "content " + id, Scope: scope,
			Lifecycle: lifecycle, Source: contextstore.SourceRef{Type: sourceType}, Metadata: metadata,
			Priority: priority, CreatedAt: created,
		}
	}
	items := []contextstore.ContextItem{
		item("shared-match-high", shared, contextstore.LifecycleCandidate, "run-1", "shared_memory_candidate", contextstore.PriorityHigh, nil),
		item("shared-match-normal", shared, contextstore.LifecycleCandidate, "run-1", "shared_memory_candidate", contextstore.PriorityNormal, nil),
		item("shared-wrong-run", shared, contextstore.LifecycleCandidate, "run-2", "shared_memory_candidate", contextstore.PriorityCritical, nil),
		item("shared-wrong-source", shared, contextstore.LifecycleCandidate, "run-1", "other", contextstore.PriorityCritical, nil),
		item("shared-wrong-lifecycle", shared, contextstore.LifecycleConfirmed, "run-1", "shared_memory_candidate", contextstore.PriorityCritical, nil),
		item("run-shared-match", session, contextstore.LifecycleCandidate, "run-1", "run_shared_context", contextstore.PriorityNormal, nil),
		item("run-shared-wrong-source", session, contextstore.LifecycleCandidate, "run-1", "other", contextstore.PriorityCritical, nil),
		item("session-match", session, contextstore.LifecycleCandidate, "run-1", "session_source", contextstore.PriorityLow, nil),
		item("session-wrong-run", session, contextstore.LifecycleCandidate, "run-2", "session_source", contextstore.PriorityCritical, nil),
		item("worker-match", worker, contextstore.LifecycleCandidate, "run-1", "worker", contextstore.PriorityNormal, map[string]string{"visibility": "private", "memory_tier": "persistent", "branch_id": "main"}),
		item("worker-wrong-tier", worker, contextstore.LifecycleCandidate, "run-1", "worker", contextstore.PriorityCritical, map[string]string{"visibility": "private", "memory_tier": "temporary", "branch_id": "main"}),
		item("worker-wrong-visibility", worker, contextstore.LifecycleCandidate, "run-1", "worker", contextstore.PriorityCritical, map[string]string{"visibility": "shared", "memory_tier": "persistent", "branch_id": "main"}),
		item("worker-wrong-branch", worker, contextstore.LifecycleCandidate, "run-1", "worker", contextstore.PriorityCritical, map[string]string{"visibility": "private", "memory_tier": "persistent", "branch_id": "sibling"}),
		item("worker-wrong-run", worker, contextstore.LifecycleCandidate, "run-2", "worker", contextstore.PriorityCritical, map[string]string{"visibility": "private", "memory_tier": "persistent", "branch_id": "main"}),
	}
	otherTeam := item("other-team", contextstore.Scope{ProjectID: "project", TeamID: "other"}, contextstore.LifecycleCandidate, "run-1", "shared_memory_candidate", contextstore.PriorityCritical, nil)
	items = append(items, otherTeam)
	if err := repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		legacy     contextstore.RepositoryQuery
		typed      contextstore.RepositoryQuery
		legacyKeep func(contextstore.ContextItem) bool
		typedKeep  func(contextstore.ContextItem) bool
	}{
		{
			name:   "shared memory settlement",
			legacy: contextstore.RepositoryQuery{Scope: shared, Visibility: contextstore.VisibilityExact, IncludeCandidates: true, Limit: 100000},
			typed:  contextstore.RepositoryQuery{Scope: shared, Visibility: contextstore.VisibilityExact, Lifecycles: []contextstore.ContextLifecycle{contextstore.LifecycleCandidate}, OriginRunID: "run-1", SourceTypes: []string{"shared_memory_candidate"}, Limit: 100000},
			legacyKeep: func(item contextstore.ContextItem) bool {
				return item.Lifecycle == contextstore.LifecycleCandidate && item.Metadata["run_id"] == "run-1" && item.Source.Type == "shared_memory_candidate"
			},
		},
		{
			name:   "run shared candidate lookup",
			legacy: contextstore.RepositoryQuery{Scope: session, Visibility: contextstore.VisibilityExact, IncludeCandidates: true, Limit: 100000},
			typed:  contextstore.RepositoryQuery{Scope: session, Visibility: contextstore.VisibilityExact, Lifecycles: []contextstore.ContextLifecycle{contextstore.LifecycleCandidate}, OriginRunID: "run-1", SourceTypes: []string{"run_shared_context"}, Limit: 100000},
			legacyKeep: func(item contextstore.ContextItem) bool {
				return item.Lifecycle == contextstore.LifecycleCandidate && item.Metadata["run_id"] == "run-1" && item.Source.Type == "run_shared_context"
			},
		},
		{
			name:   "worker memory settlement",
			legacy: contextstore.RepositoryQuery{Scope: workerRequest, Visibility: contextstore.VisibilitySubtree, IncludeCandidates: true, Limit: 100000},
			typed:  contextstore.RepositoryQuery{Scope: workerRequest, Visibility: contextstore.VisibilitySubtree, Lifecycles: []contextstore.ContextLifecycle{contextstore.LifecycleCandidate}, OriginRunID: "run-1", Limit: 100000},
			legacyKeep: func(item contextstore.ContextItem) bool {
				return item.Lifecycle == contextstore.LifecycleCandidate && item.Metadata["run_id"] == "run-1" && item.Metadata["visibility"] == "private" && promotableMemoryTier(item.Metadata["memory_tier"]) && item.Metadata["branch_id"] == "main"
			},
			typedKeep: func(item contextstore.ContextItem) bool {
				return item.Metadata["visibility"] == "private" && promotableMemoryTier(item.Metadata["memory_tier"]) && item.Metadata["branch_id"] == "main"
			},
		},
		{
			name:   "current run session prompt",
			legacy: contextstore.RepositoryQuery{Scope: session, Visibility: contextstore.VisibilityExact, IncludeCandidates: true, Limit: 100000},
			typed:  contextstore.RepositoryQuery{Scope: session, Visibility: contextstore.VisibilityExact, Lifecycles: []contextstore.ContextLifecycle{contextstore.LifecycleCandidate}, OriginRunID: "run-1", Limit: 100000},
			legacyKeep: func(item contextstore.ContextItem) bool {
				return item.Lifecycle == contextstore.LifecycleCandidate && item.Metadata["run_id"] == "run-1"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := queryAndFilterCandidateFixture(t, repo, test.legacy, test.legacyKeep)
			typed := queryAndFilterCandidateFixture(t, repo, test.typed, test.typedKeep)
			if !reflect.DeepEqual(typed, legacy) {
				t.Fatalf("typed result differs from legacy ordered items:\ntyped=%#v\nlegacy=%#v", typed, legacy)
			}
		})
	}
}

func queryAndFilterCandidateFixture(t *testing.T, repo *contextstore.SQLiteRepository, query contextstore.RepositoryQuery, keep func(contextstore.ContextItem) bool) []contextstore.ContextItem {
	t.Helper()
	items, err := repo.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if keep == nil {
		return items
	}
	filtered := make([]contextstore.ContextItem, 0, len(items))
	for _, item := range items {
		if keep(item) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}
