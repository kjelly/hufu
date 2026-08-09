package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

// newTestCoordinator builds a real Coordinator with a session whose agent map
// has the given worker count. It uses the same construction shape as the
// internal services tests (empty provider URL, temp workspace).
func newTestCoordinator(t *testing.T, workerCount int) *team.Coordinator {
	t.Helper()
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator"},
	}
	switch workerCount {
	case 0:
		// coordinator only
	case 1:
		agents["helper"] = &agent.AgentDef{Name: "Helper", Role: "worker"}
	default:
		agents["w1"] = &agent.AgentDef{Name: "w1", Role: "worker"}
		agents["w2"] = &agent.AgentDef{Name: "w2", Role: "worker"}
	}
	session := &team.TeamSession{
		Workspace: t.TempDir(),
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "test", GoalMode: "exploratory"},
		Agents:    agents,
	}
	c, err := team.NewCoordinator(session, "", "", nil, nil, nil, team.RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	return c
}

func TestShouldUseFastPath(t *testing.T) {
	if shouldUseFastPath(RouteDecision{Route: RouteFast}, nil) {
		t.Error("expected false for nil coordinator")
	}

	single := newTestCoordinator(t, 1)
	if shouldUseFastPath(RouteDecision{Route: RouteTeam}, single) {
		t.Error("expected false for team route even with single worker")
	}
	if shouldUseFastPath(RouteDecision{Route: ""}, single) {
		t.Error("expected false for empty route")
	}
	if !shouldUseFastPath(RouteDecision{Route: RouteFast}, single) {
		t.Error("expected true for fast route + single worker")
	}

	multi := newTestCoordinator(t, 2)
	if shouldUseFastPath(RouteDecision{Route: RouteFast}, multi) {
		t.Error("expected false for fast route + multiple workers (fall through to team path)")
	}

	none := newTestCoordinator(t, 0)
	if shouldUseFastPath(RouteDecision{Route: RouteFast}, none) {
		t.Error("expected false for fast route + no workers")
	}
}

func realEscalator() func(RouteDecision, int, int, bool) (bool, string) {
	return NewExecutionRouter(nil, nil).CanEscalateToTeam
}

func TestRunFastPath_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			return &team.DirectAgentResult{Output: "done", Steps: 2}, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if o.err != nil {
		t.Fatalf("unexpected err: %v", o.err)
	}
	if !o.attempted || o.escalated || o.output != "done" {
		t.Errorf("expected attempted success, got %+v", o)
	}
	if calls != 1 {
		t.Errorf("expected 1 direct call, got %d", calls)
	}
}

func TestRunFastPath_RetryThenSuccess(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			if calls == 1 {
				return &team.DirectAgentResult{Error: errors.New("transient"), Steps: 1}, nil
			}
			return &team.DirectAgentResult{Output: "done", Steps: 1}, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if o.escalated || o.output != "done" || calls != 2 {
		t.Errorf("expected retry-then-success (2 calls, not escalated), got %+v calls=%d", o, calls)
	}
}

func TestRunFastPath_EscalateOnRepeatedErrors(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			return &team.DirectAgentResult{Error: errors.New("boom"), Steps: 1}, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if !o.escalated {
		t.Errorf("expected escalation after repeated failure, got %+v", o)
	}
	if calls != 2 {
		t.Errorf("expected 2 attempts before escalation (errorCount>=2), got %d", calls)
	}
}

func TestRunFastPath_EscalateOnStepBudget(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			return &team.DirectAgentResult{Error: errors.New("ran long"), Steps: 10}, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if !o.escalated {
		t.Errorf("expected escalation on blown step budget, got %+v", o)
	}
	if calls != 1 {
		t.Errorf("expected immediate escalation (1 call), got %d", calls)
	}
}

func TestRunFastPath_EscalatesImmediatelyOnReplanRequired(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			return &team.DirectAgentResult{
				Error:          errors.New("direct agent requires replan: no-progress budget reached"),
				ReplanRequired: true,
				Steps:          1,
			}, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if !o.escalated || calls != 1 {
		t.Errorf("expected immediate team escalation after replan request, got %+v calls=%d", o, calls)
	}
}

func TestRunFastPath_TransportErrorEscalates(t *testing.T) {
	calls := 0
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			calls++
			return nil, errors.New("provider down")
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "Helper", "fix typo", RouteDecision{Route: RouteFast}, d)
	if !o.escalated {
		t.Errorf("expected escalation on repeated transport errors, got %+v", o)
	}
	if calls != 2 {
		t.Errorf("expected 2 attempts, got %d", calls)
	}
}

func TestRunFastPath_NoWorker(t *testing.T) {
	d := fastPathDispatch{
		runDirect: func(ctx context.Context, name, task string) (*team.DirectAgentResult, error) {
			t.Error("runDirect should not be called when there is no worker")
			return nil, nil
		},
		canEscalate: realEscalator(),
	}
	o := runFastPath(context.Background(), "", "fix typo", RouteDecision{Route: RouteFast}, d)
	if o.attempted || o.escalated || o.err != nil {
		t.Errorf("expected not-attempted fallthrough, got %+v", o)
	}
}
