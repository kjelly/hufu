package team

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestModelFanoutRejectsDriftBeforeProvider(t *testing.T) {
	for _, drift := range []string{"policy", "target"} {
		t.Run(drift, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "unexpected provider access", 500)
			}))
			defer server.Close()
			parent := newExecutionPolicySnapshotCoordinatorWithBackendOptions(t, t.TempDir(), 2, 3, server.URL, "", defaultExecutionPolicyCodexConfig())
			targets := []execution.ExecutionTarget{{Backend: "remote", Model: "agent-model"}, {Backend: "ollama", Model: "extra-model"}}
			item := parent.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect", Model: "remote/agent-model", ModelTopology: []string{"remote/agent-model", "ollama/extra-model"}, ExecutionTopology: targets, ExecutionTarget: targets[0]}})[0]
			parent.markAdmittedTodoIDs([]*TodoItem{item})
			task := taskDefFromTodoItem(item)
			def := cloneAgentDef(parent.session.Agents["worker"])
			def.Generation.Model = "ollama/extra-model"
			def.ExtraModels = nil
			want := "execution policy changed"
			if drift == "policy" {
				parent.session.Config.ContextRouting.QueryMode = "active"
			} else {
				task.ExecutionTopology[1].Model = "forged"
				want = "differs from parent admitted topology"
			}
			_, err := parent.executeSingleAgentWithModel(t.Context(), "worker", def, task, item.ID, "extra-1", 1)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("drift rejection: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("drift reached provider")
			}
		})
	}
}

func TestModelFanoutTargetRejectsRetargeting(t *testing.T) {
	parent := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 3)
	targets := []execution.ExecutionTarget{{Backend: "remote", Model: "agent-model"}, {Backend: "ollama", Model: "extra-model"}}
	item := parent.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect", Model: "remote/agent-model", ModelTopology: []string{"remote/agent-model", "ollama/extra-model"}, ExecutionTopology: targets, ExecutionTarget: targets[0]}})[0]
	parent.markAdmittedTodoIDs([]*TodoItem{item})
	leaf := cloneCoordinator(parent, cloneSession(parent.session, t.TempDir()))
	leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID, index: 1, target: targets[1]}
	if target, err := leaf.modelFanoutTarget(item.ID); err != nil || target != targets[1] {
		t.Fatalf("indexed target: %v %v", target, err)
	}
	if _, err := leaf.resolveTaskExecutionModel(parent.session.Agents["worker"], TaskDef{ResolvedExecutionTarget: targets[0]}, item.ID); err == nil {
		t.Fatal("primary target accepted for extra leaf")
	}
	leaf.modelFanout.target = targets[0]
	if _, err := leaf.resolveTaskExecutionModel(parent.session.Agents["worker"], TaskDef{}, item.ID); err == nil {
		t.Fatal("binding target drift accepted")
	}
	if model, frozen := leaf.frozenTaskOccurrenceModel(item.ID); !frozen || model != "" {
		t.Fatal("invalid target silently retargeted retry")
	}
	leaf.modelFanout.target = targets[1]
	leaf.session.Config.ContextRouting.QueryMode = "active"
	if err := leaf.AdmitExecutionPolicy(); err == nil {
		t.Fatal("leaf ignored genuine policy drift")
	}
}

func TestModelFanoutCreditSharesOccurrenceCap(t *testing.T) {
	parent, repo := outcomeTestCoordinator(t, "memory-1", "memory-2")
	parent.taskTracker = NewTaskTracker()
	first := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, &VerificationResult{ExitCode: 0})
	second := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-2", ContextItemID: "memory-2", Disposition: MemoryUseApplied, Confidence: 1}}, &VerificationResult{ExitCode: 0})
	second.MemoryManifests[0].RetrievalID = "retrieval-2"
	owned := cloneTodoItem(first)
	owned.MemoryManifests = append(owned.MemoryManifests, second.MemoryManifests...)
	parent.taskTracker.TodoList().AddReserved([]*TodoItem{owned})
	var wg sync.WaitGroup
	for _, item := range []*TodoItem{first, second} {
		leaf := &Coordinator{session: parent.session, modelFanout: &modelFanoutBinding{parent: parent, taskID: item.ID}}
		wg.Go(func() {
			leaf.recordMemoryOutcomeSignal(item, "verification_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
		})
	}
	wg.Wait()
	total := 0.0
	for _, id := range []string{"memory-1", "memory-2"} {
		aggregate, err := repo.ExperienceAggregate(t.Context(), id, "memory-policy-v1")
		if err != nil {
			t.Fatal(err)
		}
		total += aggregate.PositiveWeight
	}
	if total != 1 {
		t.Fatalf("concurrent leaves spent %v credit, want one occurrence cap", total)
	}
	for _, item := range []*TodoItem{first, second} {
		leaf := &Coordinator{session: parent.session, modelFanout: &modelFanoutBinding{parent: parent, taskID: item.ID}}
		leaf.recordMemoryOutcomeSignal(item, "verification_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	}
	if recorded, err := parent.memoryOutcomeWeightForSignal(owned, "verification_passed", "positive"); err != nil || recorded != 1 {
		t.Fatalf("replay credit=%v err=%v", recorded, err)
	}
}

func TestCloneCoordinatorRoutesUsageToParentNoProgressBudget(t *testing.T) {
	parent := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) {
		cfg.Reliability = agent.ReliabilityConfig{
			MaxTokensWithoutProgress:    10,
			MaxTokensWithoutProgressSet: true,
			MaxTurnsWithoutProgressSet:  true,
			MaxTasksWithoutProgressSet:  true,
			HardEnforcement:             true,
		}
	}))
	clone := cloneCoordinator(parent, parent.session)
	clone.recordReliabilityUsage("todo-1", 1, 17)
	clone.recordReliabilityUsage("todo-1", 1, 17) // a cumulative receipt deduplicates within the clone
	clone.recordNoProgressTokens(5)               // auxiliary/sidecar usage also lands on the parent

	if got := parent.noProgressCounters().Tokens; got != 22 {
		t.Fatalf("parent no-progress tokens = %d, want 22 from clone worker + auxiliary usage", got)
	}
	if got := clone.noProgressCounters().Tokens; got != 0 {
		t.Fatalf("clone-local no-progress tokens = %d, want accounting owned by parent", got)
	}
	if stopped, reason := parent.enforceNoProgressBudget(); !stopped || reason == "" {
		t.Fatalf("parent budget enforcement = (%v, %q), want terminal stop from clone usage", stopped, reason)
	}
	if result := parent.LastRunResult(); result == nil || result.Outcome != RunOutcomePartial || result.Continuation == nil {
		t.Fatalf("parent result = %#v, want partial continuation after clone usage threshold", result)
	}
}

// ExecutionTopology/ModelTopology mean parallel fan-out, not an ordered
// fallback list: every leaf beyond the first runs concurrently. Execution
// routes must therefore store fallback candidates in a separate field.
func TestModelTopologyIsFanout(t *testing.T) {
	def := &agent.AgentDef{Name: "worker", ExtraModels: []string{" ollama/b ", "ollama/c"}}
	topology := initialTaskModelTopology(def, " ollama/a ")
	if want := []string{"ollama/a", "ollama/b", "ollama/c"}; !slices.Equal(topology, want) {
		t.Fatalf("initialTaskModelTopology = %q, want %q", topology, want)
	}

	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "fanout", ModelTopology: topology},
		{Agent: "worker", Desc: "singleton", ModelTopology: []string{"ollama/a"}},
	})
	if !c.shouldExecuteWithExtraModels(def, items[0].ID) {
		t.Error("multi-leaf topology did not fan out")
	}
	if c.shouldExecuteWithExtraModels(def, items[1].ID) {
		t.Error("singleton topology fanned out")
	}
}

// An extra-model leaf gets a private control workspace but keeps the parent
// subject root, so state-changing leaves write the same project files
// concurrently.
func TestExtraModelLeafSharesSubjectRoot(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view,write", nil, nil)
	leafWorkspace := t.TempDir()
	leaf := cloneCoordinator(c, cloneSession(c.session, leafWorkspace))
	if leaf.projectDir != c.projectDir {
		t.Fatalf("leaf projectDir = %q, want the shared parent subject root %q", leaf.projectDir, c.projectDir)
	}
	if leaf.session.Workspace != leafWorkspace || c.session.Workspace == leafWorkspace {
		t.Fatalf("leaf control workspace = %q (parent %q), want a private leaf workspace", leaf.session.Workspace, c.session.Workspace)
	}
}
