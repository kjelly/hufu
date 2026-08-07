package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	contextstore "github.com/anomalyco/hufu/internal/context"
)

type wp6Vector struct{ results []contextstore.SearchResult }

func (v wp6Vector) SearchVector(context.Context, contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	return v.results, nil
}

func wp6IDs(bundle WorkerMemoryBundle) map[string]bool {
	ids := make(map[string]bool, len(bundle.Items))
	for _, item := range bundle.Items {
		ids[item.ID] = true
	}
	return ids
}

// Parent session memory is visible only through the fork event. The same
// authorization is applied after the exact, FTS, and vector stages, so a
// future parent result from any retrieval source cannot leak into a child.
func TestWP6RecallAuthorizesParentMemoryAtForkAcrossRetrievalSources(t *testing.T) {
	repo := wp3SetupRepo(t)
	main := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker"}
	feature := main
	feature.BranchID = "feature"
	sibling := main
	sibling.BranchID = "sibling"
	before := contextstore.ContextItem{ID: "main-before", Kind: contextstore.ContextObservation, Content: "lineage token before fork", Scope: main}
	after := contextstore.ContextItem{ID: "main-after", Kind: contextstore.ContextObservation, Content: "lineage token after fork", Scope: main}
	child := contextstore.ContextItem{ID: "feature-new", Kind: contextstore.ContextObservation, Content: "lineage token feature", Scope: feature}
	other := contextstore.ContextItem{ID: "sibling-new", Kind: contextstore.ContextObservation, Content: "lineage token sibling", Scope: sibling}
	wp3AppendItems(t, repo, before, after, child, other)

	svc := NewWorkerMemoryService(repo, wp6Vector{results: []contextstore.SearchResult{{Item: after, Score: 1}}})
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker", Scope: feature, Query: `"lineage token"`,
		Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 20, MaxTokens: 5000},
		SessionLineage: []WorkerMemoryLineageBranch{
			{BranchID: "feature"},
			{BranchID: "main", RestrictItems: true, AllowedItemIDs: map[string]bool{"main-before": true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := wp6IDs(bundle)
	for _, id := range []string{"main-before", "feature-new"} {
		if !ids[id] {
			t.Errorf("lineage recall omitted %q: %v", id, ids)
		}
	}
	for _, id := range []string{"main-after", "sibling-new"} {
		if ids[id] {
			t.Errorf("lineage recall leaked %q: %v", id, ids)
		}
	}
}

func TestWP6CoordinatorUsesCheckedOutBranchAndForkMemoryIDs(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	if err := es.Append(RunEvent{ID: "memory-before", BranchID: "main", Type: "worker_memory_confirmed", Payload: []byte(`{"item_id":"memory-before"}`)}); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	feature, err := tree.CreateBranch("feature", "memory-before", es)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Append(RunEvent{ID: "memory-after", BranchID: "main", Type: "worker_memory_confirmed", Payload: []byte(`{"item_id":"memory-after"}`)}); err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = feature.ID
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}, projectDir: "project"}
	if got := c.activeBranchID(); got != feature.ID {
		t.Fatalf("active branch = %q, want %q", got, feature.ID)
	}
	if got := c.contextScope().BranchID; got != "" {
		t.Fatalf("shared context scope branch = %q, want empty", got)
	}
	lineage := c.workerMemoryLineage(feature.ID)
	if len(lineage) != 2 || lineage[0].BranchID != feature.ID || lineage[1].BranchID != "main" {
		t.Fatalf("lineage = %#v, want feature then main", lineage)
	}
	if !lineage[1].RestrictItems || !lineage[1].AllowedItemIDs["memory-before"] || lineage[1].AllowedItemIDs["memory-after"] {
		t.Fatalf("parent fork authorization = %#v, want only pre-fork memory", lineage[1])
	}
}

func TestWP6SavedSessionMemoryRecallsAcrossForkAtWorkerScope(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	svc := NewWorkerMemoryService(repo, nil)
	main := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker"}
	save := func(scope contextstore.Scope, task, summary string) contextstore.ContextItem {
		t.Helper()
		item, saveErr := svc.SaveSessionMemory(context.Background(), WorkerMemoryWriteRequest{
			WorkerID: "worker", BranchID: scope.BranchID, Scope: scope, Summary: summary, Goal: "retain lineage memory",
			TaskID: task, RunID: "run-" + task, Attempt: 1, ProducerID: "worker", Verified: true,
			Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, AutoSave: true},
		})
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if item.Scope.TaskID != "" || item.Scope.AttemptID != "" {
			t.Fatalf("saved memory %q has task/attempt child scope: %#v", item.ID, item.Scope)
		}
		return item
	}

	before := save(main, "before", "durable lineage memory before fork")
	es, err := NewEventStore(workspace, "run", "sess")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	payload := func(id string) []byte {
		data, marshalErr := json.Marshal(map[string]string{"item_id": id})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return data
	}
	if err := es.Append(RunEvent{ID: "fork", BranchID: "main", Type: "worker_memory_confirmed", Payload: payload(before.ID)}); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	feature, err := tree.CreateBranch("feature", "fork", es)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := tree.CreateBranch("sibling", "fork", es)
	if err != nil {
		t.Fatal(err)
	}
	after := save(main, "after", "durable lineage memory after fork")
	if err := es.Append(RunEvent{ID: "main-after", BranchID: "main", Type: "worker_memory_confirmed", Payload: payload(after.ID)}); err != nil {
		t.Fatal(err)
	}
	featureScope := main
	featureScope.BranchID = feature.ID
	featureItem := save(featureScope, "feature", "durable lineage memory feature only")
	if err := es.Append(RunEvent{ID: "feature-memory", BranchID: feature.ID, Type: "worker_memory_confirmed", Payload: payload(featureItem.ID)}); err != nil {
		t.Fatal(err)
	}
	siblingScope := main
	siblingScope.BranchID = sibling.ID
	siblingItem := save(siblingScope, "sibling", "durable lineage memory sibling only")
	if err := es.Append(RunEvent{ID: "sibling-memory", BranchID: sibling.ID, Type: "worker_memory_confirmed", Payload: payload(siblingItem.ID)}); err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = feature.ID
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}, projectDir: "project"}
	recall := func(scope contextstore.Scope, lineage []WorkerMemoryLineageBranch) map[string]bool {
		bundle, recallErr := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
			WorkerID: "worker", Scope: scope, SessionLineage: lineage, Query: "durable lineage memory",
			Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 20, MaxTokens: 5000},
		})
		if recallErr != nil {
			t.Fatal(recallErr)
		}
		return wp6IDs(bundle)
	}
	childIDs := recall(featureScope, c.workerMemoryLineage(feature.ID))
	for _, item := range []contextstore.ContextItem{before, featureItem} {
		if !childIDs[item.ID] {
			t.Errorf("child recall omitted %q: %v", item.ID, childIDs)
		}
	}
	for _, item := range []contextstore.ContextItem{after, siblingItem} {
		if childIDs[item.ID] {
			t.Errorf("child recall leaked %q: %v", item.ID, childIDs)
		}
	}
	parentIDs := recall(main, []WorkerMemoryLineageBranch{{BranchID: "main"}})
	for _, item := range []contextstore.ContextItem{before, after} {
		if !parentIDs[item.ID] {
			t.Errorf("parent recall omitted %q: %v", item.ID, parentIDs)
		}
	}
	for _, item := range []contextstore.ContextItem{featureItem, siblingItem} {
		if parentIDs[item.ID] {
			t.Errorf("parent recall leaked child memory %q: %v", item.ID, parentIDs)
		}
	}
}

func TestWP6PersistentPromotionRejectsWrongBranchEvidence(t *testing.T) {
	repo := wp3SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	feature := wp5Scope("worker")
	feature.BranchID = "feature"
	if _, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "worker", Scope: feature, Content: "stable branch lesson", Category: "lesson", Tier: "persistent", RunID: "same-run", TaskID: "task", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: wp5Scope("worker"), WorkerID: "worker", Manifest: wp5AcceptedManifest("same-run", "task")}); err == nil {
		t.Fatal("promotion with parent-branch evidence succeeded")
	}
}

func TestWP6PersistentMemoryRemainsBranchIndependentAfterPromotion(t *testing.T) {
	repo := wp3SetupRepo(t)
	svc := NewWorkerMemoryService(repo, nil)
	feature := wp5Scope("worker")
	feature.BranchID = "feature"
	item, err := svc.SaveCandidate(context.Background(), WorkerMemoryCandidateRequest{
		WorkerID: "worker", Scope: feature, Content: "stable reusable convention", Category: "convention", Tier: "persistent", RunID: "feature-run", TaskID: "task", Source: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Confirm(context.Background(), WorkerMemoryPromotionRequest{Scope: feature, WorkerID: "worker", Manifest: wp5AcceptedManifest("feature-run", "task")}); err != nil {
		t.Fatal(err)
	}
	main := feature
	main.BranchID = "main"
	bundle, err := svc.Recall(context.Background(), WorkerMemoryRecallRequest{
		WorkerID: "worker", Scope: main, Query: "reusable convention", Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, MaxItems: 10, MaxTokens: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !wp6IDs(bundle)[item.ID] {
		t.Fatalf("confirmed persistent memory is not branch independent: %v", wp6IDs(bundle))
	}
}

func TestWP6ContextShadowWriteRemainsBranchNeutral(t *testing.T) {
	workspace := t.TempDir()
	tree := NewSessionTree()
	feature, err := tree.CreateBranch("feature", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = feature.ID
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}, projectDir: "project", contextRepo: repo}
	c.shadowContextAppend(contextstore.ContextObservation, "branch neutral shared context", "test")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Scope.BranchID != "" {
		t.Fatalf("shadow context = %#v, want branch-neutral shared record", items)
	}
}
