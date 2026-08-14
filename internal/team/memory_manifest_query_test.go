package team

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/skill"
)

// TestWorkerManifestQueryHashMatchesRetrievalQuery is the integration regression
// for the review finding that worker manifests hashed the expanded prompt while
// reranking used the raw task goal. With a skill that expands the prompt, the
// persisted manifest must hash the exact retrieval query (task.Goal), not the
// expanded prompt, so explain-memory can bind the actual retrieval query to its
// retrieval ID (spec §5.1, §7 HF-MEM4-005).
func TestWorkerManifestQueryHashMatchesRetrievalQuery(t *testing.T) {
	workspace := t.TempDir()
	// executeTask fires autoWriteSTMASync/persistReflexionLessonAsync as
	// fire-and-forget goroutines that write into workspace; give them a beat
	// to finish before TempDir cleanup removes it out from under them.
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })

	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Append(context.Background(), contextstore.ContextItem{
		ID: "deploy-procedure", Kind: contextstore.ContextPattern, Content: "deploy the service with zero downtime",
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed,
		TrustLevel: contextstore.TrustTrusted, Priority: 10, Confidence: 1, UpdatedAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}

	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	policy.PolicyVersion = "memory-policy-v1"

	agentDef := &agent.AgentDef{Name: "worker", Role: "worker", Skills: "deploy", Generation: agent.GenerationParams{Model: "test"}}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "team", MemoryLearning: policy},
			Agents:    map[string]*agent.AgentDef{"worker": agentDef},
		},
		sessionData:     NewSession(),
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-manifest-query",
		contextRepo:     repo,
		projectDir:      "project",
		skills:          []*skill.SkillDef{{Name: "deploy", Description: "deploy the service", Content: "run the deploy steps", Path: "skills/deploy/SKILL.md"}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "deploy the service"}})[0]
	c.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: TaskResultStatusCompletedWithGaps, Source: "submitted",
			Summary: "deployed",
		})
	}}

	goal := "deploy the service"
	// Prove the skill actually expands the prompt; otherwise the hash assertions
	// below would pass trivially even with the bug.
	expanded := c.appendSkillContext(goal, agentDef, "worker", goal, item.ID)
	if expanded == goal || !strings.Contains(expanded, "run the deploy steps") {
		t.Fatalf("test setup: skill did not expand the prompt: %q", expanded)
	}

	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: goal, Recovery: RecoveryRetry}, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}

	loaded := LoadSession(workspace)
	if loaded == nil {
		t.Fatal("session.json not persisted")
	}
	var manifest *MemoryInjectionManifest
	for _, task := range loaded.Tasks {
		if task == nil {
			continue
		}
		for i := range task.MemoryManifests {
			manifest = &task.MemoryManifests[i]
		}
	}
	if manifest == nil {
		t.Fatal("no memory injection manifest persisted")
	}
	if manifest.QueryHash != QueryHash(goal) {
		t.Fatalf("manifest query hash = %q, want %q (retrieval query %q)", manifest.QueryHash, QueryHash(goal), goal)
	}
	if manifest.QueryHash == QueryHash(expanded) {
		t.Fatal("manifest hashed the expanded prompt instead of the retrieval query")
	}
}
