package team

import (
	"context"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestRuntimeResultTransportRemovesReportingAuthority(t *testing.T) {
	def := testWorkerDef()
	def.Tools = "view,submit_result"
	task := TaskDef{Agent: def.Name, Execution: ExecutionContract{RequiresResult: true}}
	resolved, err := ResolveStaticWorkerTools(StaticToolResolutionInput{Agent: def, Task: task, BaseTools: []string{"view", submitResultToolName}, ResultTransport: agent.ResultTransportRuntime})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.ResultRequired || resolved.ResultToolRequired || slices.Contains(resolved.Names, submitResultToolName) || !slices.Contains(resolved.Names, "view") {
		t.Fatalf("runtime surface=%+v", resolved)
	}
	c := newTestCoordinator(t)
	ctx := context.WithValue(t.Context(), workerRuntimeResultKey{}, true)
	ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{submitResultToolName: true})
	if denial, err := c.authorizeToolInvocation(ctx, def.Name, submitResultToolName); err != nil || denial == "" {
		t.Fatalf("session permission re-enabled reporting: denial=%q err=%v", denial, err)
	}
	task.Execution.ToolSequence = []string{"view", submitResultToolName}
	if _, err := ResolveStaticWorkerTools(StaticToolResolutionInput{Agent: def, Task: task, BaseTools: []string{"view"}, ResultTransport: agent.ResultTransportRuntime}); err == nil {
		t.Fatal("explicit runtime silently weakened a closed sequence")
	}
}

func TestResultTransportUsesFrozenStaticPolicy(t *testing.T) {
	c, item, _, _ := resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", true)
	pinned := c.ExecutionPolicySnapshot().ResultCollection
	task := taskDefFromTodoItem(item)
	for _, scenario := range []struct {
		name    string
		policy  *ExecutionResultCollectionPolicySnapshot
		closed  bool
		want    agent.ResultTransport
		wantErr bool
	}{
		{name: "historical absent policy", want: agent.ResultTransportTool},
		{name: "explicit agent runtime", policy: cloneExecutionResultCollectionPolicy(pinned), want: agent.ResultTransportRuntime},
		{name: "explicit runtime conflicts with closed sequence", policy: cloneExecutionResultCollectionPolicy(pinned), closed: true, wantErr: true},
		{name: "implicit runtime preserves closed sequence", policy: func() *ExecutionResultCollectionPolicySnapshot {
			p := cloneExecutionResultCollectionPolicy(pinned)
			p.Default = agent.ResultTransportRuntime
			p.DefaultExplicit = false
			p.Agents = nil
			return p
		}(), closed: true, want: agent.ResultTransportTool},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			local := task
			if scenario.closed {
				local.Execution.ToolSequence = []string{submitResultToolName}
			}
			got, err := resultTransportFromPolicy(scenario.policy, "worker", local)
			if (err != nil) != scenario.wantErr || !scenario.wantErr && got != scenario.want {
				t.Fatalf("transport=%s err=%v", got, err)
			}
		})
	}
}

func TestResultTransportDefaultsAndAgentPrecedence(t *testing.T) {
	for _, scenario := range []struct {
		team, worker agent.ResultTransport
		want         agent.ResultTransport
	}{
		{want: agent.ResultTransportRuntime},
		{team: agent.ResultTransportTool, want: agent.ResultTransportTool},
		{team: agent.ResultTransportRuntime, worker: agent.ResultTransportTool, want: agent.ResultTransportTool},
		{team: agent.ResultTransportTool, worker: agent.ResultTransportRuntime, want: agent.ResultTransportRuntime},
	} {
		c := newTestCoordinator(t, withTeamConfig(func(config *agent.TeamConfig) { config.ResultTransport = scenario.team }))
		c.session.Agents["worker"].ResultTransport = scenario.worker
		policy, err := executionPolicyResultCollection(c.session, executionPolicySnapshotVersion)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resultTransportFromPolicy(policy, "worker", TaskDef{Agent: "worker"})
		if err != nil || got != scenario.want {
			t.Fatalf("team=%s agent=%s got=%s err=%v", scenario.team, scenario.worker, got, err)
		}
	}
}

func TestRuntimeResultTransportPreservesPlanSubmissionSurface(t *testing.T) {
	c, item, _, _ := resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", true, func(c *Coordinator) { c.session.Agents["worker"].ResultTransport = "" })
	def := c.session.Agents["worker"]
	for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionInitialPlan, WorkerToolResolutionApprovedPlan} {
		task := TaskDef{Agent: "worker", PlanFirst: true, ResolvedExecutionTarget: item.ExecutionTarget, Execution: ExecutionContract{RequiresResult: true}}
		if mode == WorkerToolResolutionApprovedPlan {
			task.PlanID = item.ID
		}
		resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: task, TodoID: item.ID, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(resolved.Names, submitResultToolName) || slices.Contains(resolved.Names, "submit_plan") != (mode == WorkerToolResolutionInitialPlan) {
			t.Fatalf("mode=%s tools=%v", mode, resolved.Names)
		}
	}
}

func TestBoundArtifactPolicyPreflightUsesResultTransportAndLifecycle(t *testing.T) {
	for _, transport := range []agent.ResultTransport{"", agent.ResultTransportTool} {
		for _, phase := range []string{"work", "initial_plan", "approved_plan", "closed_sequence"} {
			t.Run(string(transport)+"/"+phase, func(t *testing.T) {
				c, item, _, _ := resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", true, func(c *Coordinator) {
					c.session.Config.ResultTransport = transport
					c.session.Agents["worker"].ResultTransport = ""
				})
				item.WorksetBinding = &WorksetBinding{WorksetID: "workset-1", ItemKey: "one"}
				c.taskTracker.TodoList().Restore([]*TodoItem{item})
				def := c.session.Agents["worker"]
				task := taskDefFromTodoItem(item)
				if phase == "initial_plan" || phase == "approved_plan" {
					task.PlanFirst = true
					if phase == "approved_plan" {
						task.PlanID = item.ID
					}
				}
				if phase == "closed_sequence" {
					task.Execution.ToolSequence = []string{submitResultToolName}
				}
				resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
					Task: task, TodoID: item.ID, Mode: workerToolResolutionModeForTask(task),
				})
				if err != nil {
					t.Fatal(err)
				}
				scope := &ArtifactAccessScope{TaskID: item.ID, Attempt: 1}
				if err := c.validateBoundWorkerToolPolicy(t.Context(), resolved, def, task, item.ID, scope); err != nil {
					t.Fatal(err)
				}
				required := ""
				switch {
				case phase == "initial_plan":
					required = "submit_plan"
				case transport == agent.ResultTransportTool || phase == "closed_sequence":
					required = submitResultToolName
				}
				if required == "" && slices.Contains(resolved.Names, submitResultToolName) {
					t.Fatal("runtime collection granted submit_result")
				}
				// A custom resolver cannot omit a required authentic protocol
				// handler. Runtime collection legitimately needs no such handler.
				err = c.validateBoundWorkerToolPolicy(t.Context(), ResolvedWorkerTools{}, def, task, item.ID, scope)
				if (err != nil) != (required != "") {
					t.Fatalf("required=%s missing handler error=%v", required, err)
				}
			})
		}
	}
}
