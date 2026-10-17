package team

import (
	"context"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestVerifyPrivateMemoryQueryIsDeniedAtEveryBoundary(t *testing.T) {
	for _, phase := range []Phase{PhaseVerify, "verify", PhaseExecute} {
		t.Run(string(phase), func(t *testing.T) {
			c, _ := newWorkerMemoryQueryCoordinator(t, agent.WorkerMemorySession)
			def := c.agentDefByName("critic")
			def.Tools = "view,memory_query"
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: def.Name, Desc: "confirm finding", Phase: phase}})[0]
			ctx := context.WithValue(t.Context(), todoIDKey{}, item.ID)
			ctx = tools.SetToolsAllowed(ctx, []string{"memory_query"})
			ctx = context.WithValue(ctx, tools.AgentNameKey, def.Name)
			tool := &canonicalMemoryQueryTool{coordinator: c}
			c.coreTools = []fantasy.AgentTool{tool}
			selected := agentToolNames(c.selectWorkerToolsForTask(def, TaskDef{Agent: def.Name, Phase: phase, SideEffect: SideEffectWorkspaceWrite}))
			verify := strings.EqualFold(string(phase), string(PhaseVerify))
			if slices.Contains(selected, "memory_query") == verify {
				t.Fatalf("phase %s exposes tools %v", phase, selected)
			}
			call := fantasy.ToolCall{ID: "query", Name: "memory_query", Input: `{"query":"review finding","n":10}`}
			for _, boundary := range []struct {
				name string
				tool fantasy.AgentTool
			}{
				{name: "handler", tool: tool},
				{name: "policy", tool: c.gatePolicyTools([]fantasy.AgentTool{tool})[0]},
			} {
				response, err := boundary.tool.Run(ctx, call)
				if err != nil || response.IsError != verify {
					t.Fatalf("%s phase %s: response=%+v error=%v", boundary.name, phase, response, err)
				}
				if verify && (strings.Contains(response.Content, "critic-own") || !strings.Contains(response.Content, "VERIFY")) {
					t.Fatalf("%s disclosed private memory: %s", boundary.name, response.Content)
				}
				if !verify && !strings.Contains(response.Content, "critic-own") {
					t.Fatalf("%s EXECUTE lost private memory: %s", boundary.name, response.Content)
				}
			}
		})
	}
}

func TestVerifyMemoryPolicyOverridesSessionToolPermission(t *testing.T) {
	c, _ := newWorkerMemoryQueryCoordinator(t, agent.WorkerMemorySession)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", Desc: "verify", Phase: PhaseVerify}})[0]
	ctx := context.WithValue(t.Context(), todoIDKey{}, item.ID)
	ctx = tools.SetToolsAllowed(ctx, []string{"memory_query"})
	ctx = context.WithValue(ctx, tools.AgentToolsSessionPermissionsKey, map[string]bool{"memory_query": true})
	inner := &recordingTool{name: "memory_query"}
	response, err := c.gatePolicyTools([]fantasy.AgentTool{inner})[0].Run(ctx, fantasy.ToolCall{ID: "override", Name: "memory_query", Input: `{"query":"secret"}`})
	if err != nil || !response.IsError || inner.ran {
		t.Fatalf("session permission bypassed VERIFY: response=%+v err=%v ran=%v", response, err, inner.ran)
	}
}
