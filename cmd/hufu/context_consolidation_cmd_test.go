package main

import (
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContradictoryExperiencesAreNotMerged(t *testing.T) {
	items := []contextstore.ContextItem{
		consolidationTestItem("a", "project-a", map[string]string{"contradicts_ids": "b"}),
		consolidationTestItem("b", "project-a", nil),
	}
	if err := validateConsolidationSources(items, "project-a", "team"); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestCrossProjectWideningRequiresReview(t *testing.T) {
	items := []contextstore.ContextItem{
		consolidationTestItem("a", "project-a", nil),
		consolidationTestItem("b", "project-b", nil),
	}
	if err := validateConsolidationSources(items, "project-a", "team"); err == nil || !strings.Contains(err.Error(), "widen") {
		t.Fatalf("validation error = %v", err)
	}
}

func consolidationTestItem(id, project string, metadata map[string]string) contextstore.ContextItem {
	return contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: id, Scope: contextstore.Scope{ProjectID: project, TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: metadata}
}
