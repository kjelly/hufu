package inspect

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestProjectTaskIncludesCatalogAction(t *testing.T) {
	item := &team.TodoItem{
		ID: "task-1", SideEffect: team.SideEffectWorkspaceWrite,
		CatalogAction: &team.CatalogActionBinding{ActionID: "restart", EntryHash: "sha256:e", ArgumentsHash: "sha256:a", InvocationID: "tai_x", ProposalIDs: []string{"tap_1"}},
	}
	data := projectTask(item, InspectQuery{RunID: "run-1", TaskID: item.ID})
	if data.SideEffect != "workspace_write" || data.CatalogAction == nil || data.CatalogAction.ActionID != "restart" ||
		data.CatalogAction.InvocationID != "tai_x" || len(data.CatalogAction.ProposalIDs) != 1 {
		t.Fatalf("catalog projection = %#v / %#v", data.SideEffect, data.CatalogAction)
	}
	data.CatalogAction.ProposalIDs[0] = "changed"
	if item.CatalogAction.ProposalIDs[0] != "tap_1" {
		t.Fatal("inspect projection aliases the catalog binding")
	}
	if plain := projectTask(&team.TodoItem{ID: "task-2"}, InspectQuery{RunID: "run-1", TaskID: "task-2"}); plain.CatalogAction != nil || plain.SideEffect != "" {
		t.Fatalf("non-catalog projection = %#v", plain)
	}
}
