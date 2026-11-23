package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepositorySnapshotBoundsRelatedEvidenceAndPlansEveryPrimary(t *testing.T) {
	repo := newFixtureRepo(t)
	const target = "service/a_target.go"
	const callerCount = 64
	for i := range callerCount {
		name := fmt.Sprintf("service/caller_%02d_%s.go", i, strings.Repeat("context_", 8))
		writeFile(t, filepath.Join(repo, name), fmt.Sprintf("package service\nfunc Caller%02d() { Target() }\n", i))
	}
	writeAndCommit(t, repo, target, "package service\nfunc Target() {}\n", "related evidence must fit", "2025-01-02T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "snapshot-out")
	config.Routing = routingDocumentation
	config.Scope = resolverScope{Kind: "repository_snapshot", History: "first_parent", Head: "HEAD"}
	config.MaxCommits = 0
	config.MaxPaths = 1
	config.MaxReviewInputTokens = 12000
	if _, err := Prepare(t.Context(), config); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(repo, config.OutputDir)
	var inventory repositorySnapshotManifest
	data, err := os.ReadFile(filepath.Join(out, "repository-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	planned := make(map[string]int64)
	for _, workset := range inventory.Plan.Worksets {
		for _, source := range workset.Sources {
			if len(source.Spans) == 1 && source.Spans[0].Start == 0 {
				planned[source.Path] = source.Spans[0].End
			}
		}
	}
	var inventoried int
	for _, file := range inventory.Files {
		if !strings.HasPrefix(file.Path, "service/") {
			continue
		}
		inventoried++
		if planned[file.Path] != file.Bytes || file.SHA256 == "" {
			t.Errorf("source lost primary coverage or identity: %s", file.Path)
		}
	}
	if inventoried != 1+callerCount {
		t.Fatalf("snapshot inventory lost source files: %d", inventoried)
	}

	manifest := readManifest(t, filepath.Join(out, "primary", "workset-manifest.json"))
	for _, entry := range manifest.Items {
		if len(entry.TouchedPaths) != 1 || entry.TouchedPaths[0] != target {
			continue
		}
		assertReviewInputBudget(t, repo, entry, config)
		for _, input := range entry.Inputs {
			if input.Kind != "review_evidence_inventory" {
				continue
			}
			var evidence evidenceInventory
			data, err := os.ReadFile(filepath.Join(repo, input.Path))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &evidence); err != nil {
				t.Fatal(err)
			}
			if len(evidence.Sources) != 1+maxSnapshotRelatedPaths || evidence.Sources[0].Path != target || evidence.Sources[1].Role != "related" {
				t.Fatalf("unexpected workset evidence count or primary: %d", len(evidence.Sources))
			}
			return
		}
	}
	t.Fatal("target did not publish its bounded evidence inventory")
}

func TestRepositorySnapshotPaginatesFinalWorksets(t *testing.T) {
	repo := newFixtureRepo(t)
	for i := range 29 {
		writeFile(t, filepath.Join(repo, fmt.Sprintf("a%02d", i), "small.txt"), "small complete source\n")
	}
	const largePath = "z/large.txt"
	writeAndCommit(t, repo, largePath, strings.Repeat("單行也能切分", 501), "force a cross-batch split", "2025-01-02T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "")
	config.Routing = routingDocumentation
	config.MaxCommits = 0
	config.MaxWorksetItems = 32
	config.MaxSourceSnapshotBytes = 4096
	config.Scope = resolverScope{Kind: "repository_snapshot", History: "first_parent", Head: "HEAD"}
	var firstPlan []snapshotPlannedWorkset
	for page := range 2 {
		config.Scope.Batch = page + 1
		config.OutputDir = strings.Repeat("snapshot", page+1)
		result, err := Prepare(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		scope := result.Outputs["scope"].(scopeAttestation).RepositorySnapshot
		if scope.Batch != page+1 || scope.BatchCount != 2 || scope.WorksetsPerBatch != 30 || scope.Plan != nil {
			t.Fatalf("incorrect compact batch metadata: %+v", scope)
		}
		out := filepath.Join(repo, config.OutputDir)
		data, err := os.ReadFile(filepath.Join(out, "repository-snapshot.json"))
		if err != nil {
			t.Fatal(err)
		}
		var inventory repositorySnapshotManifest
		if err := json.Unmarshal(data, &inventory); err != nil {
			t.Fatal(err)
		}
		plan := inventory.Plan
		if plan == nil || len(plan.Worksets) <= 30 {
			t.Fatal("the large file did not increase the number of worksets")
		}
		if page == 0 {
			firstPlan = plan.Worksets
		} else if !reflect.DeepEqual(firstPlan, plan.Worksets) {
			t.Fatal("different requested pages changed the final partition plan")
		}
		primary := readManifest(t, filepath.Join(out, "primary", "workset-manifest.json"))
		wantPrimary := min(30, len(plan.Worksets)-page*30)
		if len(primary.Items) != wantPrimary || result.Outputs["item_count"] != wantPrimary+2 || len(plan.BatchSourceSHA256) != wantPrimary {
			t.Fatalf("page %d: primary=%d total=%v hashes=%d", page+1, len(primary.Items), result.Outputs["item_count"], len(plan.BatchSourceSHA256))
		}
		if primary.Items[wantPrimary-1].TouchedPaths[0] != largePath {
			t.Fatalf("page %d does not contain a fragment of %s", page+1, largePath)
		}
		for i, entry := range primary.Items {
			assertReviewInputBudget(t, repo, entry, config)
			for _, input := range entry.Inputs {
				if input.Kind == "review_source_snapshot" && input.SHA256 != plan.BatchSourceSHA256[i] {
					t.Fatalf("workset %s hash differs from the inventory plan", entry.Key)
				}
			}
		}
	}
	config.Scope.Batch = 3
	config.OutputDir = "out-of-range"
	if _, err := Prepare(t.Context(), config); err == nil || !strings.Contains(err.Error(), "exceeds batch_count 2") {
		t.Fatalf("invalid batch was not rejected: %v", err)
	}
}

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
