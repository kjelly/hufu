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
