package context

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRepositoryQueryTypedPredicates(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()

	scope := Scope{ProjectID: "project", TeamID: "team"}
	created := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	items := []ContextItem{
		{ID: "match", Kind: ContextObservation, Content: "match", Scope: scope, Lifecycle: LifecycleCandidate, Source: SourceRef{Type: "run_shared_context"}, Metadata: map[string]string{"run_id": "run-1"}, CreatedAt: created},
		{ID: "wrong-run", Kind: ContextObservation, Content: "wrong run", Scope: scope, Lifecycle: LifecycleCandidate, Source: SourceRef{Type: "run_shared_context"}, Metadata: map[string]string{"run_id": "run-2"}, CreatedAt: created},
		{ID: "wrong-source", Kind: ContextObservation, Content: "wrong source", Scope: scope, Lifecycle: LifecycleCandidate, Source: SourceRef{Type: "shared_memory_candidate"}, Metadata: map[string]string{"run_id": "run-1"}, CreatedAt: created},
		{ID: "confirmed", Kind: ContextObservation, Content: "confirmed", Scope: scope, Lifecycle: LifecycleConfirmed, Source: SourceRef{Type: "run_shared_context"}, Metadata: map[string]string{"run_id": "run-1"}, CreatedAt: created},
		{ID: "literal", Kind: ContextObservation, Content: "literal", Scope: scope, Lifecycle: LifecycleCandidate, Source: SourceRef{Type: "run_shared_context"}, Metadata: map[string]string{"run_id": "run-1' OR 1=1 --"}, CreatedAt: created},
	}
	if err := repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}

	query := RepositoryQuery{
		Scope:       scope,
		Visibility:  VisibilityExact,
		Lifecycles:  []ContextLifecycle{LifecycleCandidate, LifecycleCandidate},
		OriginRunID: "run-1",
		SourceTypes: []string{" run_shared_context ", "run_shared_context", ""},
	}
	got, err := repo.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOfItems(got); !reflect.DeepEqual(ids, []string{"match"}) {
		t.Fatalf("typed query ids = %v, want [match]", ids)
	}

	query.OriginRunID = "run-1' OR 1=1 --"
	got, err = repo.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsOfItems(got); !reflect.DeepEqual(ids, []string{"literal"}) {
		t.Fatalf("bound origin run ids = %v, want [literal]", ids)
	}
}

func TestRepositoryQueryTypedPredicateValidation(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	scope := Scope{ProjectID: "project"}

	tests := []RepositoryQuery{
		{Scope: scope, IncludeCandidates: true, Lifecycles: []ContextLifecycle{LifecycleCandidate}},
		{Scope: scope, Lifecycles: []ContextLifecycle{""}},
		{Scope: scope, Lifecycles: []ContextLifecycle{"unknown"}},
		{Scope: scope, SourceTypes: []string{" ", "\t"}},
	}
	for i, query := range tests {
		if _, err := repo.Query(t.Context(), query); err == nil {
			t.Fatalf("invalid query %d unexpectedly succeeded", i)
		}
	}
}

func TestRepositoryIterateHasNoImplicitLimitAndPreservesOrder(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	scope := Scope{ProjectID: "project"}
	created := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	items := make([]ContextItem, 105)
	for i := range items {
		items[i] = ContextItem{ID: fmt.Sprintf("item-%03d", i), Kind: ContextObservation, Content: fmt.Sprintf("item %d", i), Scope: scope, CreatedAt: created}
	}
	if err := repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}

	var got []string
	err = repo.Iterate(t.Context(), RepositoryQuery{Scope: scope}, func(item ContextItem) error {
		got = append(got, item.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 105 || got[0] != "item-000" || got[len(got)-1] != "item-104" {
		t.Fatalf("iteration returned %d ids in unexpected order: first=%q last=%q", len(got), got[0], got[len(got)-1])
	}
}

func TestRepositoryIterateContract(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(t.Context(), ContextItem{ID: "item", Kind: ContextObservation, Content: "item", Scope: scope}); err != nil {
		t.Fatal(err)
	}

	if err := repo.Iterate(t.Context(), RepositoryQuery{Scope: scope, Limit: 1}, func(ContextItem) error { return nil }); err == nil {
		t.Fatal("non-zero iteration limit unexpectedly succeeded")
	}
	if err := repo.Iterate(t.Context(), RepositoryQuery{Scope: scope}, nil); err == nil {
		t.Fatal("nil iteration visitor unexpectedly succeeded")
	}
	want := errors.New("stop")
	err = repo.Iterate(t.Context(), RepositoryQuery{Scope: scope}, func(ContextItem) error { return want })
	if err != want {
		t.Fatalf("visitor error = %v, want unchanged sentinel", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := repo.Iterate(canceled, RepositoryQuery{Scope: scope}, func(ContextItem) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled iteration error = %v, want context.Canceled", err)
	}
}

func TestReadOnlyRepositoryUsesTypedPredicateAndIterationContracts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	writer, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{ProjectID: "project"}
	if err := writer.Append(t.Context(), ContextItem{
		ID: "candidate", Kind: ContextObservation, Content: "candidate", Scope: scope,
		Lifecycle: LifecycleCandidate, Source: SourceRef{Type: "run_shared_context"}, Metadata: map[string]string{"run_id": "run-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	invalid := RepositoryQuery{Scope: scope, IncludeCandidates: true, Lifecycles: []ContextLifecycle{LifecycleCandidate}}
	if _, err := reader.Query(t.Context(), invalid); err == nil {
		t.Fatal("read-only Query accepted an invalid lifecycle combination")
	}
	if err := reader.Iterate(t.Context(), RepositoryQuery{Scope: scope, Limit: 1}, func(ContextItem) error { return nil }); err == nil {
		t.Fatal("read-only Iterate accepted a non-zero limit")
	}
	var got []string
	err = reader.Iterate(t.Context(), RepositoryQuery{
		Scope: scope, Lifecycles: []ContextLifecycle{LifecycleCandidate}, OriginRunID: "run-1", SourceTypes: []string{"run_shared_context"},
	}, func(item ContextItem) error {
		got = append(got, item.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"candidate"}) {
		t.Fatalf("read-only iteration ids = %v, want [candidate]", got)
	}
}

func TestCandidateSettlementQueryPlanUsesCanonicalScopeIndex(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	query := RepositoryQuery{
		Scope: Scope{ProjectID: "project", TeamID: "team"}, Visibility: VisibilityExact,
		Lifecycles: []ContextLifecycle{LifecycleCandidate}, OriginRunID: "run-1", SourceTypes: []string{"run_shared_context"},
	}
	where, args, err := compileRepositoryPredicates(query, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := repo.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT "+itemColumns+" FROM context_items WHERE "+strings.Join(where, " AND ")+" ORDER BY priority DESC, created_at DESC, id ASC", args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(details, "\n")
	if !strings.Contains(joined, "idx_context_scope") {
		t.Fatalf("candidate settlement query plan does not use canonical scope index:\n%s", joined)
	}
	t.Log(joined)
}
