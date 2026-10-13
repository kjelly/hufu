package context

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAgentActivationIterationPreservesCanonicalScopeAndProjection(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "owner"}
	persistent, foreign, sibling, shared := scope, scope, scope, scope
	persistent.SessionID, persistent.BranchID = "", ""
	foreign.AgentID, sibling.BranchID, shared.AgentID = "foreign", "sibling", ""
	for id, recordScope := range map[string]Scope{"session": scope, "persistent": persistent, "foreign": foreign, "sibling": sibling, "shared": shared} {
		if err := repo.Append(t.Context(), ContextItem{ID: id, Scope: recordScope, Content: "canonical " + id, Kind: ContextPattern, Metadata: map[string]string{"activation.phases": "execute", "activation.roles": "worker"}}); err != nil {
			t.Fatal(err)
		}
	}
	var legacy []ContextItem
	query := RepositoryQuery{Scope: scope}
	if err := repo.Iterate(t.Context(), query, func(item ContextItem) error {
		if item.Scope.AgentID == scope.AgentID {
			legacy = append(legacy, item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 2 {
		t.Fatalf("bad authorization control: %d items", len(legacy))
	}
	type activated struct {
		item   ContextItem
		record ActivationRecord
	}
	expected := make(map[string]activated)
	for _, item := range legacy {
		record, err := repo.ActivationForItem(t.Context(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		expected[item.ID] = activated{item, record}
	}
	got := make(map[string]activated)
	if err := repo.IterateAgentActivations(t.Context(), query, func(item ContextItem, record ActivationRecord) error {
		got[item.ID] = activated{item, record}
		return nil
	}); err != nil || !reflect.DeepEqual(expected, got) {
		t.Fatalf("canonical/projection parity failed: err=%v, ids=%v", err, got)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := repo.IterateAgentActivations(cancelled, query, func(ContextItem, ActivationRecord) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	if _, err := repo.db.ExecContext(t.Context(), "DELETE FROM context_activation WHERE context_item_id=?", "session"); err != nil {
		t.Fatal(err)
	}
	if err := repo.IterateAgentActivations(t.Context(), query, func(ContextItem, ActivationRecord) error { return nil }); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing typed projection did not fail closed: %v", err)
	}
}
