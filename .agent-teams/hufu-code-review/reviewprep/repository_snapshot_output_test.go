package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositorySnapshotLargeInventoryUsesArtifact(t *testing.T) {
	repo := newFixtureRepo(t)
	for i := range 4000 {
		writeFile(t, filepath.Join(repo, "docs", fmt.Sprintf("%04d-%s.md", i, strings.Repeat("inventory", 15))), "excluded documentation\n")
	}
	writeAndCommit(t, repo, "target.go", "package sample\nfunc Target() {}\n", "large inventory", "2025-01-02T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "snapshot-out")
	config.Routing = routingDocumentation
	config.Scope = resolverScope{Kind: "repository_snapshot", History: "first_parent", Head: "HEAD"}
	config.MaxCommits = 0
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) >= 900*1024 {
		t.Fatalf("action bytes=%d err=%v", len(encoded), err)
	}
	scope := result.Outputs["scope"].(scopeAttestation)
	if len(scope.RepositorySnapshot.Files) != 0 {
		t.Fatal("complete inventory leaked into action output")
	}
	data, err := os.ReadFile(filepath.Join(repo, "snapshot-out", "repository-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 900*1024 {
		t.Fatalf("fixture inventory must exceed the action budget: %d", len(data))
	}
	var inventory repositorySnapshotManifest
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Files) != 4002 {
		t.Fatalf("inventory lost files: %d", len(inventory.Files))
	}
}
