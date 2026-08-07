package team

import (
	"context"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func newDelegationPolicyCoordinator(policy agent.DelegationPolicy) *Coordinator {
	return &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Delegation: policy}},
		taskTracker: NewTaskTracker(),
	}
}

func TestDelegationPolicyRejectsOneTaskBeforeConfiguredInitialBatch(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"reader", "probe"},
		RequireExactInitialBatch: true,
	})

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "reader", Goal: "read"}})
	if err == nil || !strings.Contains(err.Error(), "first delegation must contain exactly") {
		t.Fatalf("expected initial-batch rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("rejected initial batch created %d TODOs, want none", got)
	}

	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "reader", Goal: "read"}, {Agent: "probe", Goal: "inspect"}}); err != nil {
		t.Fatalf("configured initial batch was rejected: %v", err)
	}
}

func TestDelegationPolicyRejectsRedispatchAfterSuccessfulTerminalResult(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		NoRedispatchAfterSuccess: []string{"reader", "probe"},
	})
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader", Desc: "read contract"}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "typed success")

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "reader", Goal: "duplicate read"},
		{Agent: "probe", Goal: "independent probe"},
	})
	if err == nil || !strings.Contains(err.Error(), "may not be redispatched") {
		t.Fatalf("expected successful-worker redispatch rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 1 {
		t.Fatalf("rejected duplicate batch changed existing TODOs: got %d, want 1", got)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("successful task status changed to %s", got)
	}
}
