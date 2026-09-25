package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func coordinatorAgentContext() context.Context {
	ctx := tools.SetToolsAllowed(context.Background(), []string{"agent", teamActionGetToolName, teamActionListToolName, "finish"})
	return context.WithValue(ctx, todoIDKey{}, CoordTodoID)
}

func runAgentTool(t *testing.T, tool fantasy.AgentTool, input string) (fantasy.ToolResponse, error) {
	t.Helper()
	return tool.Run(coordinatorAgentContext(), fantasy.ToolCall{ID: fmt.Sprintf("call-%d", len(input)), Name: "agent", Input: input})
}

func catalogAgentInput(id, arguments string) string {
	return fmt.Sprintf(`{"tasks":[{"agent":"runtime-engineer","goal":"run %s","catalog_action":{"id":%q,"arguments":%s}}]}`, id, id, arguments)
}

func catalogTodos(c *Coordinator) []*TodoItem {
	var items []*TodoItem
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.CatalogAction != nil {
			items = append(items, item)
		}
	}
	return items
}

func TestAgentToolDispatchesACatalogAction(t *testing.T) {
	c, provider := dispatchTestCoordinator(t, "", nil)
	response, err := runAgentTool(t, c.RunAgentsTool(), catalogAgentInput("collect-debug-bundle", `{"count":3,"service":"api"}`))
	if err != nil || response.IsError {
		t.Fatalf("dispatch = %#v, err %v", response, err)
	}
	items := catalogTodos(c)
	if len(items) != 1 {
		t.Fatalf("catalog todos = %d, want 1", len(items))
	}
	item := items[0]
	if item.Status != TaskDone || item.Action == nil || item.Action.Payload != `{"count":3,"service":"api"}` || item.DecisionProfile != DecisionProfileOff ||
		!strings.HasPrefix(item.CatalogAction.InvocationID, "tai_") || item.ResultContract != nil {
		t.Fatalf("catalog todo = %#v", item)
	}
	if provider.executed != 1 || provider.env.CatalogInvocationID != item.CatalogAction.InvocationID {
		t.Fatalf("provider executions = %d, invocation %q", provider.executed, provider.env.CatalogInvocationID)
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, event := range events {
		if event.Type == string(EventTaskCreated) && strings.Contains(string(event.Payload), `"catalog_action"`) {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("task_created events with catalog_action = %d, want 1", created)
	}

	// A worker that depends on the catalog task receives its runtime outputs.
	worker := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "network-engineer", Desc: "use the bundle", DependsOn: []string{item.ID}}})[0]
	dependencies := c.dependencyResultsForTask(worker.ID)
	if len(dependencies) != 1 || dependencies[0].Source != "runtime" || dependencies[0].RuntimeOutputs["summary"] != "ok" {
		t.Fatalf("dependency results = %#v, want the catalog action's outputs", dependencies)
	}

	// The budget (max 2) counts admitted todos and survives a rebuilt list.
	if response, err := runAgentTool(t, c.RunAgentsTool(), catalogAgentInput("collect-debug-bundle", `{"service":"db"}`)); err != nil || response.IsError {
		t.Fatalf("second dispatch = %#v, err %v", response, err)
	}
	c.taskTracker.TodoList().Restore(ReduceToTodoList(mustReadEvents(t, c.eventStore)))
	response, err = runAgentTool(t, c.RunAgentsTool(), catalogAgentInput("collect-debug-bundle", `{"service":"cache"}`))
	if err != nil || !response.IsError || !strings.Contains(response.Content, teamActionBudgetExceeded) {
		t.Fatalf("third dispatch = %#v, err %v, want the budget rejection", response, err)
	}
}

func TestCatalogDispatchRejectionsAreRecoverable(t *testing.T) {
	c, provider := dispatchTestCoordinator(t, "", nil)
	gated := c.gatePolicyTools([]fantasy.AgentTool{c.RunAgentsTool()})[0]
	for attempt := range maxTeamActionRejections {
		response, err := runAgentTool(t, gated, catalogAgentInput("missing-action", fmt.Sprintf(`{"n":%d}`, attempt)))
		if err != nil || !response.IsError || !strings.HasPrefix(response.Content, teamActionDispatchRejectedPrefix) || !strings.Contains(response.Content, teamActionUnknown) {
			t.Fatalf("rejection %d = %#v, err %v", attempt+1, response, err)
		}
		if c.coordinatorPolicyRepairPending.Load() {
			t.Fatalf("rejection %d entered policy repair", attempt+1)
		}
	}
	response, err := runAgentTool(t, gated, catalogAgentInput("missing-action", `{"n":99}`))
	if err != nil || !strings.Contains(response.Content, coordinatorPolicyRepairPrefix) || !c.coordinatorPolicyRepairPending.Load() {
		t.Fatalf("fourth rejection = %#v, err %v, want policy repair", response, err)
	}
	if len(c.taskTracker.TodoList().Items()) != 0 || provider.executed != 0 {
		t.Fatalf("rejections created %d todos and ran the provider %d times", len(c.taskTracker.TodoList().Items()), provider.executed)
	}
}

func TestCatalogDispatchRejectionUsesPolicyRepairWhenRepairOrWrapUpIsActive(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(*Coordinator)
	}{
		{name: "repair pending", prepare: func(c *Coordinator) { c.coordinatorPolicyRepairPending.Store(true) }},
		{name: "wrap-up", prepare: func(c *Coordinator) { c.wrapUp.Store(1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := dispatchTestCoordinator(t, "", nil)
			tt.prepare(c)
			response, err := c.RunAgentsTool().Run(coordinatorAgentContext(), fantasy.ToolCall{ID: "c", Name: "agent", Input: catalogAgentInput("missing-action", `{}`)})
			if strings.HasPrefix(response.Content, teamActionDispatchRejectedPrefix) || !strings.Contains(response.Content, coordinatorPolicyRepairPrefix) {
				t.Fatalf("%s rejection = %#v, err %v, want the policy repair path", tt.name, response, err)
			}
		})
	}
}

func TestCatalogDispatchRejectionKeepsCompletedWorkersDispatchable(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	response, err := c.RunAgentsTool().Run(coordinatorAgentContext(), fantasy.ToolCall{ID: "c", Name: "agent", Input: catalogAgentInput("collect-debug-bundle", `{"service":7}`)})
	if err != nil || !strings.Contains(response.Content, teamActionArgumentsInvalid) {
		t.Fatalf("rejection = %#v, err %v", response, err)
	}
	if c.coordinatorPolicyRepairsAttempt.Load() != 0 || c.coordinatorPolicyRepairPending.Load() {
		t.Fatal("a catalog rejection consumed policy repair budget")
	}
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "network-engineer", Goal: "redo"}}); err != nil {
		t.Fatalf("delegation after a catalog rejection = %v", err)
	}
}
